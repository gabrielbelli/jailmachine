package cli

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/gabrielbelli/jailmachine/internal/backend"
	"github.com/gabrielbelli/jailmachine/internal/machine"
	"github.com/gabrielbelli/jailmachine/internal/netprov"
)

// fakeSysctl stands in for the guest's sysctl(8), with the OpenZFS rules
// the ARC program has to get past: a runtime arc.max at or below the floor
// (c_min), under 64 MiB or not below the memory is refused, and an arc.max
// of 0 is accepted but leaves c_max alone. Writes are logged to "calls".
const fakeSysctl = `#!/bin/sh
d="$JM_FAKE_SYSCTL"
if [ "$1" = -n ]; then
  [ -f "$d/$2" ] || exit 1
  cat "$d/$2"
  exit 0
fi
echo "$*" >> "$d/calls"
name=${1%%=*}; val=${1#*=}
cmin=$(cat "$d/kstat.zfs.misc.arcstats.c_min")
cmax=$(cat "$d/kstat.zfs.misc.arcstats.c_max")
pm=$(cat "$d/hw.physmem")
case "$name" in
vfs.zfs.arc.max)
  if [ "$val" != 0 ] && { [ "$val" -lt 67108864 ] || [ "$val" -le "$cmin" ] || [ "$val" -ge "$pm" ]; }; then
    echo "sysctl: $name=$val: Invalid argument" >&2; exit 1
  fi
  [ "$val" = 0 ] || echo "$val" > "$d/kstat.zfs.misc.arcstats.c_max" ;;
vfs.zfs.arc.min)
  if [ "$val" -lt 33554432 ] || [ "$val" -gt "$cmax" ]; then
    echo "sysctl: $name=$val: Invalid argument" >&2; exit 1
  fi
  echo "$val" > "$d/kstat.zfs.misc.arcstats.c_min" ;;
*) exit 1 ;;
esac
`

// fakeGuest is the state fakeSysctl serves: memory, ARC floor and ceiling in
// bytes, and loader.conf's content ("" with noConf means no file).
type fakeGuest struct {
	dir string
}

func newFakeGuest(t *testing.T, physmem, cmin, cmax int64, loaderConf string, haveConf bool) *fakeGuest {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh on this host")
	}
	dir := t.TempDir()
	write := func(name, content string, mode os.FileMode) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	write("bin/sysctl", fakeSysctl, 0o755)
	write("hw.physmem", strconv.FormatInt(physmem, 10), 0o644)
	write("kstat.zfs.misc.arcstats.c_min", strconv.FormatInt(cmin, 10), 0o644)
	write("kstat.zfs.misc.arcstats.c_max", strconv.FormatInt(cmax, 10), 0o644)
	if haveConf {
		write("loader.conf", loaderConf, 0o644)
	}
	return &fakeGuest{dir: dir}
}

// apply runs arcScriptAt against the fake guest and returns the exit status
// and stderr. The write log is cleared first.
func (g *fakeGuest) apply(t *testing.T, arcMiB int) (int, string) {
	t.Helper()
	_ = os.Remove(filepath.Join(g.dir, "calls"))
	cmd := exec.Command("sh", "-c", arcScriptAt(arcMiB, filepath.Join(g.dir, "loader.conf")))
	cmd.Env = append(os.Environ(), "JM_FAKE_SYSCTL="+g.dir, "PATH="+filepath.Join(g.dir, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
	var errb strings.Builder
	cmd.Stderr = &errb
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0, errb.String()
	case errors.As(err, &exit):
		return exit.ExitCode(), errb.String()
	default:
		t.Fatal(err)
		return -1, ""
	}
}

func (g *fakeGuest) read(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(g.dir, name))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(data))
}

const (
	gib = int64(1) << 30
	mib = int64(1) << 20
)

func TestArcScriptCapsAndPersists(t *testing.T) {
	g := newFakeGuest(t, 2*gib, 64*mib, 2*gib*5/8, "zfs_load=\"YES\"\n", true)
	if rc, errOut := g.apply(t, 512); rc != 0 {
		t.Fatalf("exit %d: %s", rc, errOut)
	}
	if got := g.read(t, "kstat.zfs.misc.arcstats.c_max"); got != "536870912" {
		t.Errorf("c_max = %s", got)
	}
	if got := g.read(t, "kstat.zfs.misc.arcstats.c_min"); got != "67108864" {
		t.Errorf("the floor was moved although it was below the cap: %s", got)
	}
	wantConf := "zfs_load=\"YES\"\nvfs.zfs.arc.max=\"536870912\""
	if got := g.read(t, "loader.conf"); got != wantConf {
		t.Errorf("loader.conf =\n%s\nwant\n%s", got, wantConf)
	}
	// Idempotent: a second run writes nothing.
	if rc, errOut := g.apply(t, 512); rc != 0 {
		t.Fatalf("second run: exit %d: %s", rc, errOut)
	}
	if calls := g.read(t, "calls"); calls != "" {
		t.Errorf("second run wrote sysctls: %s", calls)
	}
	if got := g.read(t, "loader.conf"); got != wantConf {
		t.Errorf("second run changed loader.conf:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(g.dir, "loader.conf.jm")); !os.IsNotExist(err) {
		t.Errorf("temporary file left behind: %v", err)
	}
}

func TestArcScriptLowersTheFloorOnABigGuest(t *testing.T) {
	// 32 GiB: the ARC's floor is 1 GiB, above a 512 MiB cap.
	g := newFakeGuest(t, 32*gib, gib, 31*gib, "", true)
	if rc, errOut := g.apply(t, 512); rc != 0 {
		t.Fatalf("exit %d: %s", rc, errOut)
	}
	if got := g.read(t, "kstat.zfs.misc.arcstats.c_max"); got != "536870912" {
		t.Errorf("c_max = %s", got)
	}
	if got := g.read(t, "kstat.zfs.misc.arcstats.c_min"); got != "268435456" {
		t.Errorf("c_min = %s, want half the cap", got)
	}
}

func TestArcScriptRestoresTheDefault(t *testing.T) {
	for _, c := range []struct {
		name          string
		physmem, want int64
	}{
		// FreeBSD's arc_default_max: the larger of 5/8 of the memory and
		// the memory less 1 GiB.
		{"2 GiB", 2 * gib, 2 * gib * 5 / 8},
		{"32 GiB", 32 * gib, 31 * gib},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := newFakeGuest(t, c.physmem, 256*mib, 512*mib,
				"vfs.zfs.arc.max=\"536870912\"\nzfs_load=\"YES\"\n  vfs.zfs.arc.max = 1\n", true)
			if rc, errOut := g.apply(t, 0); rc != 0 {
				t.Fatalf("exit %d: %s", rc, errOut)
			}
			if got := g.read(t, "kstat.zfs.misc.arcstats.c_max"); got != strconv.FormatInt(c.want, 10) {
				t.Errorf("c_max = %s, want %d: the old cap is still in force", got, c.want)
			}
			if got := g.read(t, "loader.conf"); got != `zfs_load="YES"` {
				t.Errorf("loader.conf =\n%s", got)
			}
			if rc, _ := g.apply(t, 0); rc != 0 || g.read(t, "calls") != "" {
				t.Errorf("second run: exit %d, calls %q", rc, g.read(t, "calls"))
			}
		})
	}
}

func TestArcScriptCreatesLoaderConf(t *testing.T) {
	g := newFakeGuest(t, 2*gib, 64*mib, 2*gib*5/8, "", false)
	if rc, errOut := g.apply(t, 1024); rc != 0 {
		t.Fatalf("exit %d: %s", rc, errOut)
	}
	if got := g.read(t, "loader.conf"); got != `vfs.zfs.arc.max="1073741824"` {
		t.Errorf("loader.conf = %q", got)
	}
	// Removing a cap from a guest with no loader.conf creates nothing.
	g = newFakeGuest(t, 2*gib, 64*mib, 2*gib*5/8, "", false)
	if rc, errOut := g.apply(t, 0); rc != 0 {
		t.Fatalf("exit %d: %s", rc, errOut)
	}
	if _, err := os.Stat(filepath.Join(g.dir, "loader.conf")); !os.IsNotExist(err) {
		t.Errorf("loader.conf created for no cap: %v", err)
	}
}

func TestArcScriptPersistsARefusedCap(t *testing.T) {
	// The runtime sysctl is refused (the cap is not below the memory), but
	// loader.conf is still written, an older cap replaced, and the exit
	// status says something went wrong.
	g := newFakeGuest(t, 256*mib, 32*mib, 160*mib, "vfs.zfs.arc.max=\"1073741824\"\nzfs_load=\"YES\"\n", true)
	rc, errOut := g.apply(t, 512)
	if rc == 0 || !strings.Contains(errOut, "Invalid argument") {
		t.Errorf("exit %d, stderr %q", rc, errOut)
	}
	if got, want := g.read(t, "loader.conf"), "zfs_load=\"YES\"\nvfs.zfs.arc.max=\"536870912\""; got != want {
		t.Errorf("loader.conf =\n%s\nwant\n%s", got, want)
	}
}

func TestArcScriptAvoidsSysrc(t *testing.T) {
	// sysrc(8) dies on any name with a dot in it.
	for _, arc := range []int{0, 512} {
		if got := arcScript(arc); strings.Contains(got, "sysrc") || !strings.Contains(got, "/boot/loader.conf") {
			t.Errorf("arcScript(%d):\n%s", arc, got)
		}
	}
}

func TestArcScriptLargeCap(t *testing.T) {
	// 64 GiB overflows 32 bits; the byte count must not wrap.
	if got := arcScript(64 << 10); !strings.Contains(got, "vfs.zfs.arc.max=68719476736") {
		t.Errorf("arcScript(64GiB):\n%s", got)
	}
}

func TestValidateArc(t *testing.T) {
	for _, c := range []struct {
		arc, mem int
		ok       bool
	}{
		{0, 2048, true},
		{64, 2048, true},
		{512, 2048, true},
		{2047, 2048, true},
		{63, 2048, false},
		{2048, 2048, false},
		{4096, 2048, false},
		{0, 256, true},
	} {
		if err := validateArc(c.arc, c.mem); (err == nil) != c.ok {
			t.Errorf("validateArc(%d, %d) = %v, want ok=%v", c.arc, c.mem, err, c.ok)
		}
	}
}

func TestArcWord(t *testing.T) {
	if got := arcWord(0); got != "guest default" {
		t.Errorf("arcWord(0) = %q", got)
	}
	if got := arcWord(512); got != "512 MiB" {
		t.Errorf("arcWord(512) = %q", got)
	}
}

func TestSetValidateArc(t *testing.T) {
	m := machine.Defaults() // 2048 MiB, 512 MiB cap
	cases := []struct {
		name string
		o    setOpts
		want string // substring of the error; "" means valid
		arc  int
	}{
		{"arc ok", setOpts{arc: "1GiB", arcSet: true}, "", 1024},
		{"arc zero", setOpts{arc: "0", arcSet: true}, "", 0},
		{"arc tiny", setOpts{arc: "32", arcSet: true}, "--arc must", 0},
		{"arc at memory", setOpts{arc: "2048", arcSet: true}, "--arc must", 0},
		{"arc junk", setOpts{arc: "half", arcSet: true}, "--arc:", 0},
		// --memory in the same call is what --arc is compared with.
		{"arc with new memory", setOpts{arc: "3g", arcSet: true, memory: "4g", memorySet: true}, "", 3072},
		{"arc above new memory", setOpts{arc: "1g", arcSet: true, memory: "1g", memorySet: true}, "--arc must", 0},
		// Lowering the memory under the recorded cap is refused rather
		// than warned about at every start.
		{"memory under cap", setOpts{memory: "512", memorySet: true}, "--arc", 0},
	}
	for _, tc := range cases {
		c, err := tc.o.validate(&m)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: unexpected error %v", tc.name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: error %v, want %q", tc.name, err, tc.want)
		case tc.want == "" && c.arcMiB != tc.arc:
			t.Errorf("%s: parsed %d, want %d", tc.name, c.arcMiB, tc.arc)
		}
	}
	c, _ := setOpts{arc: "1g", arcSet: true}.validate(&m)
	if c.needsStopped() {
		t.Error("--arc should not need a stopped machine")
	}
}

func TestSetArcStoppedMachine(t *testing.T) {
	root := t.TempDir()
	seedRecord(t, root, "alpha")
	out, err := run(t, root, "set", "alpha", "--arc", "0")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	m, err := machine.NewStore(root).Load("alpha")
	if err != nil || m.ArcMiB != 0 {
		t.Fatalf("record not updated: %+v, %v", m, err)
	}
	if !strings.Contains(out, "next start") {
		t.Errorf("output should say the cap applies on the next start:\n%s", out)
	}
	out, err = run(t, root, "inspect", "alpha")
	if err != nil || !strings.Contains(out, "ZFS ARC cap:") || !strings.Contains(out, "guest default") {
		t.Errorf("inspect = %q, %v", out, err)
	}
	if _, err := run(t, root, "set", "alpha", "--arc", "2g"); err == nil || !strings.Contains(err.Error(), "--arc must") {
		t.Errorf("cap at the memory size accepted: %v", err)
	}
}

func TestInspectArcRow(t *testing.T) {
	root := t.TempDir()
	seedRecord(t, root, "dev")
	out, err := run(t, root, "inspect", "dev")
	if err != nil || !strings.Contains(out, "ZFS ARC cap:") || !strings.Contains(out, "512 MiB") {
		t.Errorf("inspect = %q, %v", out, err)
	}
	out, _ = run(t, root, "inspect", "--help")
	if !strings.Contains(out, "arc_mib") {
		t.Error("inspect help should document arc_mib")
	}
}

func TestInitValidateArc(t *testing.T) {
	base := initOpts{cpus: 4, memory: 2048, disk: 64, sshPort: 2222}
	for arc, ok := range map[string]bool{"": true, "512": true, "0": true, "1g": true, "64": true, "2048": false, "63": false, "lots": false} {
		o := base
		o.arc = arc
		err := o.validate()
		if (err == nil) != ok {
			t.Errorf("init --arc %q: %v, want ok=%v", arc, err, ok)
		}
		if err != nil && !strings.Contains(err.Error(), "--arc") {
			t.Errorf("init --arc %q: error %v does not name the flag", arc, err)
		}
	}
	o := base
	o.arc, o.memory = "1g", 4096
	if mib, err := o.arcMiB(); err != nil || mib != 1024 {
		t.Errorf("init --arc 1g: %d, %v", mib, err)
	}
	// Without --arc the cap follows a small --memory instead of refusing it.
	for mem, want := range map[int]int{2048: 512, 512: 256, 256: 128} {
		o := base
		o.memory = mem
		if err := o.validate(); err != nil {
			t.Errorf("init --memory %d: %v", mem, err)
		}
		if mib, err := o.arcMiB(); err != nil || mib != want {
			t.Errorf("init --memory %d: cap %d, %v; want %d", mem, mib, err, want)
		}
	}
	o = base
	o.memory, o.arc = 512, "512"
	if err := o.validate(); err == nil || !strings.Contains(err.Error(), "--arc") {
		t.Errorf("init --memory 512 --arc 512: %v", err)
	}
}

// startArcServer runs an in-process SSH server that accepts key, records
// each command it is asked to run, and exits 0.
func startArcServer(t *testing.T, key ssh.PublicKey) (int, func() []string) {
	t.Helper()
	_, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
			if string(k.Marshal()) == string(key.Marshal()) {
				return &ssh.Permissions{}, nil
			}
			return nil, errors.New("unknown key")
		},
	}
	cfg.AddHostKey(hostSigner)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var mu sync.Mutex
	var cmds []string
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				sc, chans, reqs, err := ssh.NewServerConn(c, cfg)
				if err != nil {
					return
				}
				defer sc.Close()
				go ssh.DiscardRequests(reqs)
				for nc := range chans {
					ch, creqs, err := nc.Accept()
					if err != nil {
						return
					}
					for r := range creqs {
						if r.Type != "exec" {
							r.Reply(false, nil)
							continue
						}
						var p struct{ Cmd string }
						_ = ssh.Unmarshal(r.Payload, &p)
						mu.Lock()
						cmds = append(cmds, p.Cmd)
						mu.Unlock()
						r.Reply(true, nil)
						_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
						ch.Close()
						break
					}
				}
			}()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), cmds...)
	}
}

func TestSetArcRunningMachine(t *testing.T) {
	root := t.TempDir()
	seedFakeRecord(t, root, "live")
	fakeBE.state, fakeNet.state = backend.Running, backend.Running
	t.Cleanup(func() {
		fakeBE.state, fakeNet.state, fakeNet.endpoint = backend.Stopped, backend.Stopped, nil
	})

	// No SSH key and nothing listening: the cap is saved first, and the
	// error says it was not applied.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	fakeNet.endpoint = &netprov.Endpoint{SSHHost: "127.0.0.1", SSHPort: closed}
	_, err = run(t, root, "set", "live", "--arc", "1g")
	if err == nil || !strings.Contains(err.Error(), "recorded but not applied") {
		t.Fatalf("unreachable guest: %v", err)
	}
	if m, err := machine.NewStore(root).Load("live"); err != nil || m.ArcMiB != 1024 {
		t.Fatalf("record not saved before the apply: %+v, %v", m, err)
	}

	// A reachable guest gets the ARC program for the new cap.
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	keyPath := machine.NewStore(root).Path("live", machine.SSHKeyFile)
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	port, cmds := startArcServer(t, sshPub)
	fakeNet.endpoint = &netprov.Endpoint{SSHHost: "127.0.0.1", SSHPort: port}
	out, err := run(t, root, "set", "live", "--arc", "0")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if strings.Contains(out, "next start") {
		t.Errorf("a running machine's cap should not wait for the next start:\n%s", out)
	}
	got := cmds()
	if len(got) != 1 || got[0] != arcScript(0) {
		t.Errorf("guest ran %q, want the arcScript(0) program", got)
	}
	if m, err := machine.NewStore(root).Load("live"); err != nil || m.ArcMiB != 0 {
		t.Errorf("record: %+v, %v", m, err)
	}
}

func TestSSHDScript(t *testing.T) {
	for _, want := range []string{
		"grep -qx 'MaxAuthTries 20' \"$f\" && grep -qx 'ClientAliveInterval 30' \"$f\" && grep -qx 'ClientAliveCountMax 4' \"$f\" && exit 0",
		"jm_set MaxAuthTries 20 &&",
		"jm_set ClientAliveInterval 30 &&",
		"jm_set ClientAliveCountMax 4 &&",
		"sshd -t || { rc=$?; mv \"$f.jm\" \"$f\"; exit $rc; }",
		"service sshd reload",
	} {
		if !strings.Contains(sshdSettingsScript, want) {
			t.Errorf("sshd script lacks %q:\n%s", want, sshdSettingsScript)
		}
	}
	if out, err := exec.Command("/bin/sh", "-n", "-c", sshdSettingsScript).CombinedOutput(); err != nil {
		t.Errorf("sh -n: %v: %s", err, out)
	}

	// Run it against a scratch config with fake sshd and service: the
	// commented default is replaced, a missing line appended, a second run
	// changes nothing, and a config sshd -t rejects is put back.
	dir := t.TempDir()
	conf := filepath.Join(dir, "sshd_config")
	orig := "#MaxAuthTries 6\nPermitRootLogin prohibit-password\n#ClientAliveInterval 0\n"
	if err := os.WriteFile(conf, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	// The script is written for FreeBSD's sed, whose -i takes a separate
	// (here empty) suffix; GNU sed would read '' as a file. This sed turns
	// that into the attached-suffix form both accept.
	sed := `#!/bin/sh
if [ "$1" = -i ] && [ "$2" = "" ]; then
  shift 2
  for a; do last=$a; done
  PATH=/usr/bin:/bin sed -i.jmsed "$@" || exit
  rm -f "$last.jmsed"
  exit 0
fi
PATH=/usr/bin:/bin exec sed "$@"
`
	for name, body := range map[string]string{
		"sshd":    "#!/bin/sh\n[ ! -e \"" + filepath.Join(dir, "reject") + "\" ]\n",
		"service": "#!/bin/sh\nexit 0\n",
		"sed":     sed,
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	script := strings.Replace(sshdSettingsScript, "f=/etc/ssh/sshd_config", "f="+conf, 1)
	runScript := func() (string, error) {
		cmd := exec.Command("/bin/sh", "-c", script)
		cmd.Env = append(os.Environ(), "PATH="+bin+":/usr/bin:/bin")
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	if out, err := runScript(); err != nil || !strings.Contains(out, "changed") {
		t.Fatalf("first run = %q, %v", out, err)
	}
	data, _ := os.ReadFile(conf)
	for _, kv := range sshdSettings {
		if strings.Count(string(data), kv+"\n") != 1 {
			t.Errorf("config lacks exactly one %q:\n%s", kv, data)
		}
	}
	if out, err := runScript(); err != nil || strings.Contains(out, "changed") {
		t.Errorf("second run = %q, %v", out, err)
	}
	if err := os.WriteFile(conf, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "reject"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runScript(); err == nil {
		t.Error("a config sshd -t rejects was accepted")
	}
	if data, _ := os.ReadFile(conf); string(data) != orig {
		t.Errorf("rejected config not put back:\n%s", data)
	}
	if _, err := os.Stat(conf + ".jm"); !os.IsNotExist(err) {
		t.Errorf("backup left behind: %v", err)
	}
}
