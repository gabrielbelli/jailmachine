package seed

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kdomanski/iso9660"
)

const testKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGtestkeytestkeytestkeytestkeytestkeytestkey belli@mac"

const testScript = "#!/bin/sh\n# first-boot provisioning\nexec > /var/log/jm-provision.log 2>&1\necho \"$JM_HOSTNAME\"\ntouch /var/db/jm-provisioned\n"

func params() Params {
	return Params{
		InstanceID:      "jailmachine",
		Hostname:        "jailmachine",
		SSHPubKey:       testKey,
		ProvisionScript: testScript,
	}
}

// readISO opens the image and returns its label and a map of root-level
// file name to contents.
func readISO(t *testing.T, path string) (string, map[string]string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	img, err := iso9660.OpenImage(f)
	if err != nil {
		t.Fatalf("open image: %v", err)
	}
	label, err := img.Label()
	if err != nil {
		t.Fatalf("label: %v", err)
	}
	root, err := img.RootDir()
	if err != nil {
		t.Fatalf("root dir: %v", err)
	}
	children, err := root.GetChildren()
	if err != nil {
		t.Fatalf("children: %v", err)
	}
	files := map[string]string{}
	for _, c := range children {
		if c.IsDir() {
			continue
		}
		data, err := io.ReadAll(c.Reader())
		if err != nil {
			t.Fatalf("read %s: %v", c.Name(), err)
		}
		files[c.Name()] = string(data)
	}
	return label, files
}

func TestBuild(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "nested", "seed.iso")
	if err := Build(dest, params()); err != nil {
		t.Fatalf("Build: %v", err)
	}

	info, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() == 0 || info.Size()%2048 != 0 {
		t.Errorf("iso size %d is not a positive multiple of 2048", info.Size())
	}

	label, files := readISO(t, dest)
	if strings.TrimSpace(label) != VolumeID {
		t.Errorf("volume id = %q, want %q", label, VolumeID)
	}

	wantMeta := "instance-id: jailmachine\nlocal-hostname: jailmachine\n"
	if got := files[MetaDataFile]; got != wantMeta {
		t.Errorf("meta-data = %q, want %q", got, wantMeta)
	}

	wantUser := "#!/bin/sh\n" +
		"JM_SSH_PUBKEY='" + testKey + "'\n" +
		"JM_HOSTNAME='jailmachine'\n" +
		"export JM_SSH_PUBKEY JM_HOSTNAME\n" +
		"# first-boot provisioning\nexec > /var/log/jm-provision.log 2>&1\necho \"$JM_HOSTNAME\"\ntouch /var/db/jm-provisioned\n"
	got, ok := files[UserDataFile]
	if !ok {
		t.Fatalf("user-data missing; files present: %v", keys(files))
	}
	if got != wantUser {
		t.Errorf("user-data mismatch\n got: %q\nwant: %q", got, wantUser)
	}
	if !strings.HasPrefix(got, "#!/bin/sh\n") {
		t.Errorf("user-data does not start with #!/bin/sh")
	}
	if strings.Count(got, "#!/bin/sh") != 1 {
		t.Errorf("user-data contains more than one shebang")
	}
}

func TestBuildOverwritesExisting(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "seed.iso")
	if err := os.WriteFile(dest, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Build(dest, params()); err != nil {
		t.Fatalf("Build: %v", err)
	}
	label, _ := readISO(t, dest)
	if strings.TrimSpace(label) != VolumeID {
		t.Errorf("volume id = %q", label)
	}
	entries, _ := os.ReadDir(filepath.Dir(dest))
	if len(entries) != 1 {
		t.Errorf("temp file left behind: %v", entries)
	}
}

func TestUserDataScriptWithoutShebang(t *testing.T) {
	p := params()
	p.ProvisionScript = "echo hi\n"
	got := UserData(p)
	if !strings.HasSuffix(got, "export JM_SSH_PUBKEY JM_HOSTNAME\necho hi\n") {
		t.Errorf("unexpected user-data: %q", got)
	}
}

func TestUserDataTrimsKeyWhitespace(t *testing.T) {
	p := params()
	p.SSHPubKey = "  " + testKey + "  "
	if !strings.Contains(UserData(p), "JM_SSH_PUBKEY='"+testKey+"'\n") {
		t.Errorf("key not trimmed: %q", UserData(p))
	}
}

func TestValidateRejectsUnsafeInput(t *testing.T) {
	cases := map[string]func(*Params){
		"empty instance id":      func(p *Params) { p.InstanceID = "" },
		"newline in instance id": func(p *Params) { p.InstanceID = "a\nb" },
		"empty hostname":         func(p *Params) { p.Hostname = "" },
		"quote in hostname":      func(p *Params) { p.Hostname = "jail'machine" },
		"empty key":              func(p *Params) { p.SSHPubKey = "" },
		"quote in key":           func(p *Params) { p.SSHPubKey = "ssh-ed25519 AAAA x'; rm -rf / #" },
		"newline in key":         func(p *Params) { p.SSHPubKey = testKey + "\nevil" },
		"empty script":           func(p *Params) { p.ProvisionScript = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			p := params()
			mutate(&p)
			err := Build(filepath.Join(t.TempDir(), "seed.iso"), p)
			if !errors.Is(err, ErrInvalidParams) {
				t.Errorf("err = %v, want ErrInvalidParams", err)
			}
		})
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
