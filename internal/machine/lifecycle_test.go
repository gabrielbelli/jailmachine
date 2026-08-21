package machine

import (
	"errors"
	"testing"
)

func TestResolveName(t *testing.T) {
	got, err := ResolveName(nil)
	if err != nil || got != DefaultName {
		t.Fatalf("ResolveName(nil) = %q, %v", got, err)
	}
	got, err = ResolveName([]string{"e2e"})
	if err != nil || got != "e2e" {
		t.Fatalf("ResolveName(e2e) = %q, %v", got, err)
	}
	for _, bad := range []string{"Upper", "-lead", "a_b", "a.b", "with space", "../x"} {
		if _, err := ResolveName([]string{bad}); !errors.Is(err, ErrInvalidName) {
			t.Errorf("ResolveName(%q) err = %v, want ErrInvalidName", bad, err)
		}
	}
}

func TestParseImageRef(t *testing.T) {
	cases := map[string]ImageRef{
		"":                      {Source: DefaultImage},
		"official":              {Source: "official"},
		"official:15.1-RELEASE": {Source: "official", Release: "15.1-RELEASE"},
	}
	for in, want := range cases {
		got, err := ParseImageRef(in)
		if err != nil || got != want {
			t.Errorf("ParseImageRef(%q) = %+v, %v; want %+v", in, got, err, want)
		}
		if in != "" && got.String() != in {
			t.Errorf("String() = %q, want %q", got.String(), in)
		}
	}
	for _, bad := range []string{":15.1", "official:a/b", "official:a b"} {
		if _, err := ParseImageRef(bad); !errors.Is(err, ErrInvalidImage) {
			t.Errorf("ParseImageRef(%q) err = %v, want ErrInvalidImage", bad, err)
		}
	}
}

func TestEndpoints(t *testing.T) {
	m := Defaults()
	if got := m.SSHEndpoint(); got != "127.0.0.1:2222" {
		t.Errorf("SSHEndpoint = %q", got)
	}
	want := "ssh://root@127.0.0.1:2222/var/run/podman/podman.sock"
	if got := m.PodmanURI(); got != want {
		t.Errorf("PodmanURI = %q, want %q", got, want)
	}
}

func TestStageError(t *testing.T) {
	base := errors.New("boom")
	err := NewStageError(StageSSH, "see console.log", base)
	if !errors.Is(err, base) {
		t.Fatal("StageError does not unwrap")
	}
	var se *StageError
	if !errors.As(err, &se) || se.Stage != StageSSH {
		t.Fatalf("errors.As failed: %v", err)
	}
	if got := err.Error(); got != `stage "ssh" failed: boom (see console.log)` {
		t.Errorf("Error() = %q", got)
	}
}
