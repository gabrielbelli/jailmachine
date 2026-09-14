package qemu

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The fake QEMU's QMP side is a small state machine: enough of stop, cont,
// quit, migrate to a file and an incoming load to drive the suspend engine.
// Every command it receives is appended to $JM_FAKE_QEMU_RECORD with what the
// machine directory looked like at that moment, so tests can assert on
// ordering against the journal and the image.
const (
	fakeStatusEnv  = "JM_FAKE_QEMU_STATUS" // initial query-status
	fakeMigEnv     = "JM_FAKE_QEMU_MIG"    // initial query-migrate status
	fakeMigrateEnv = "JM_FAKE_MIGRATE"     // "" (completes), fail, hang, die, short, blocked
	fakeLoadEnv    = "JM_FAKE_LOAD"        // "" (loads), fail, exit, hang
	fakeRecordEnv  = "JM_FAKE_QEMU_RECORD"
)

// fakeRecord is one command as the fake saw it.
type fakeRecord struct {
	Cmd     string          `json:"cmd"`
	Args    json.RawMessage `json:"args,omitempty"`
	PID     int             `json:"pid"`
	Status  string          `json:"status"`  // query-status before the command
	Journal string          `json:"journal"` // journal phase, "" if absent, "bad"
	Image   bool            `json:"image"`   // suspend.state exists
	Tmp     bool            `json:"tmp"`     // suspend.state.tmp exists
}

type fakeFrame struct {
	Execute   string          `json:"execute"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

type fakeVM struct {
	mu              sync.Mutex
	status          string
	mig, migErr     string
	dir             string
	memMiB          int64
	migrate, load   string
	rec             *os.File
	recMu           sync.Mutex
	incomingStarted bool
}

func serveFakeQEMU(mode string, args []string, pidFile, qmp string) int {
	if mode == "unsupported" {
		fmt.Fprintln(os.Stderr, `qemu-system-aarch64: unsupported machine type: "virt-99.0"`)
		fmt.Fprintln(os.Stderr, "Use -machine help to list supported machines")
		return 1
	}
	vm := &fakeVM{
		dir:     filepath.Dir(pidFile),
		status:  "running",
		mig:     os.Getenv(fakeMigEnv),
		migrate: os.Getenv(fakeMigrateEnv),
		load:    os.Getenv(fakeLoadEnv),
	}
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-m" {
			vm.memMiB, _ = strconv.ParseInt(args[i+1], 10, 64)
		}
	}
	if hasIncoming(args) {
		vm.status = "inmigrate"
	}
	if s := os.Getenv(fakeStatusEnv); s != "" {
		vm.status = s
	}
	if path := os.Getenv(fakeRecordEnv); path != "" {
		if f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
			vm.rec = f
		}
	}
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	delay, _ := strconv.Atoi(os.Getenv(fakeQEMUDelayEnv))
	time.Sleep(time.Duration(delay) * time.Millisecond)
	ln, err := net.Listen("unix", qmp)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	for {
		conn, err := ln.Accept()
		if err != nil {
			return 1
		}
		go vm.serve(conn)
	}
}

func (vm *fakeVM) serve(conn net.Conn) {
	defer conn.Close()
	enc, dec := json.NewEncoder(conn), json.NewDecoder(conn)
	_ = enc.Encode(map[string]any{"QMP": map[string]any{"version": map[string]any{}, "capabilities": []string{}}})
	for {
		var f fakeFrame
		if dec.Decode(&f) != nil {
			return
		}
		reply, after := vm.handle(f)
		_ = enc.Encode(reply)
		if after != nil {
			after()
		}
	}
}

// later runs fn under the lock after d.
func (vm *fakeVM) later(d time.Duration, fn func()) {
	go func() {
		time.Sleep(d)
		vm.mu.Lock()
		defer vm.mu.Unlock()
		fn()
	}()
}

func fileExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func (vm *fakeVM) record(f fakeFrame) {
	sp := suspendPaths(vm.dir)
	rec := fakeRecord{Cmd: f.Execute, Args: f.Arguments, PID: os.Getpid(), Status: vm.status, Image: fileExists(sp.Image), Tmp: fileExists(sp.Tmp)}
	if j, err := readJournal(sp.Journal); err == nil {
		rec.Journal = string(j.Phase)
	} else if !errors.Is(err, os.ErrNotExist) {
		rec.Journal = "bad"
	}
	if vm.rec == nil {
		return
	}
	b, _ := json.Marshal(rec)
	vm.recMu.Lock()
	_, _ = vm.rec.Write(append(b, '\n'))
	vm.recMu.Unlock()
}

func (vm *fakeVM) handle(f fakeFrame) (reply any, after func()) {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	vm.record(f)
	ok := ret(map[string]any{})
	fail := func(desc string) any {
		return map[string]any{"error": map[string]any{"class": "GenericError", "desc": desc}}
	}
	var uri struct {
		URI string `json:"uri"`
	}
	if len(f.Arguments) > 0 {
		_ = json.Unmarshal(f.Arguments, &uri)
	}
	path := strings.TrimPrefix(uri.URI, "file:")

	switch f.Execute {
	case "query-status":
		return ret(map[string]any{"status": vm.status, "running": vm.status == "running"}), nil
	case "query-version":
		return ret(map[string]any{"qemu": map[string]int{"major": 11, "minor": 1, "micro": 1}, "package": ""}), nil
	case "query-machines":
		return ret([]map[string]any{{"name": "virt-11.0"}, {"name": "virt-11.1", "alias": "virt"}}), nil
	case "query-migrate-capabilities":
		return ret([]map[string]any{{"capability": "mapped-ram", "state": false}, {"capability": "multifd", "state": false}}), nil
	case "query-migrate":
		r := map[string]any{}
		if vm.mig != "" {
			r["status"] = vm.mig
		}
		if vm.migErr != "" {
			r["error-desc"] = vm.migErr
		}
		if vm.migrate == "blocked" {
			r["blocked-reasons"] = []string{"virtio-9p: Migration is disabled when VirtFS export path '/Users/x' is mounted in the guest using mount_tag 'jm0'"}
		}
		if vm.mig == "completed" {
			r["total-time"], r["downtime"] = 9213, 19
			r["ram"] = map[string]int64{"transferred": 1300000000, "total": 2000000000}
		}
		return ret(r), nil
	case "stop":
		if vm.status == "running" {
			vm.status = "paused"
		}
		return ok, nil
	case "cont":
		if vm.status == "inmigrate" || migrationActive(vm.mig) {
			return fail("Migration is not finalized"), nil
		}
		vm.status = "running"
		return ok, nil
	case "migrate":
		vm.mig = "active"
		switch vm.migrate {
		case "fail":
			vm.later(200*time.Millisecond, func() { vm.mig, vm.migErr = "failed", "No space left on device" })
		case "hang":
		case "die":
			after = func() { time.Sleep(100 * time.Millisecond); os.Exit(1) }
		case "short":
			vm.later(100*time.Millisecond, func() {
				_ = os.Truncate(path, 1<<20)
				vm.mig, vm.status = "completed", "postmigrate"
			})
		default:
			vm.later(300*time.Millisecond, func() {
				_ = os.Truncate(path, vm.memMiB<<20)
				vm.mig, vm.status = "completed", "postmigrate"
			})
		}
		return ok, after
	case "migrate_cancel":
		if migrationActive(vm.mig) {
			vm.mig = "cancelling"
			vm.later(100*time.Millisecond, func() { vm.mig = "cancelled" })
		}
		return ok, nil
	case "migrate-incoming":
		if _, err := os.Stat(path); err != nil {
			return fail(err.Error()), nil
		}
		vm.mig = "active"
		switch vm.load {
		case "fail":
			vm.later(100*time.Millisecond, func() { vm.mig, vm.migErr = "failed", "load of migration failed: Invalid argument" })
		case "exit":
			after = func() {
				time.Sleep(100 * time.Millisecond)
				fmt.Fprintln(os.Stderr, "qemu-system-aarch64: load of migration failed: Invalid argument")
				os.Exit(1)
			}
		case "hang":
		default:
			vm.later(300*time.Millisecond, func() { vm.mig, vm.status = "completed", "paused" })
		}
		return ok, after
	case "quit":
		return ok, func() { os.Exit(0) }
	}
	return ok, nil
}
