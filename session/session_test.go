package session

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

const testPrefix = "test-session-v1:"

var uuidV5Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-5[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestUUID_ShapeAndDeterminism(t *testing.T) {
	d := New(Namespace(testPrefix, "persona"))
	a := d.UUID("C123", "1716000000.000100")
	b := d.UUID("C123", "1716000000.000100")
	if a != b {
		t.Fatalf("same inputs gave different UUIDs: %s vs %s", a, b)
	}
	if !uuidV5Pattern.MatchString(a) {
		t.Fatalf("not a v5-shaped UUID: %s", a)
	}
	if d.UUID("C123", "x") == d.UUID("C999", "x") {
		t.Fatal("different channels collided")
	}
}

// The core invariant: a top-level message and a reply in its thread derive the
// SAME session — that's how a thread resumes. A thread's root ts equals the
// top-level message's ts.
func TestKey_ThreadResumesTopLevel(t *testing.T) {
	d := New(Namespace(testPrefix, "persona"))
	rootTS := "1716000000.000100"
	topLevel := d.Key("C1", "" /*threadTS*/, rootTS /*messageTS*/, "")
	reply := d.Key("C1", rootTS /*threadTS*/, "1716000000.000200" /*messageTS*/, "")
	if topLevel != reply {
		t.Fatalf("thread reply must resume the top-level session: %s vs %s", topLevel, reply)
	}
}

func TestKey_DistinctTopLevelMessagesAreDistinctSessions(t *testing.T) {
	d := New(Namespace(testPrefix, "persona"))
	a := d.Key("C1", "", "1716000000.000100", "")
	b := d.Key("C1", "", "1716000000.000200", "")
	if a == b {
		t.Fatal("two distinct top-level messages should be distinct sessions")
	}
}

func TestKey_LoopSharesOneSession(t *testing.T) {
	d := New(Namespace(testPrefix, "persona"))
	t1 := d.Key("C1", "", "1716000000.000100", "watch-deploys")
	t2 := d.Key("C1", "", "1716000000.999999", "watch-deploys") // different ts, same loop
	if t1 != t2 {
		t.Fatal("ticks of the same loop must share one session")
	}
	if t1 == d.Key("C1", "", "1716000000.000100", "other-loop") {
		t.Fatal("different loops should be distinct sessions")
	}
}

// DMs are uniform with channels: a top-level "DM" message (no thread) keys on
// its ts exactly like a channel message — there is no channel-type special case
// in the API at all, which is the point.
func TestKey_UniformAcrossSurfaces(t *testing.T) {
	d := New(Namespace(testPrefix, "persona"))
	// Same (channel, ts) → same key regardless of how the caller thinks of the
	// surface; Key takes no channelType, so DMs cannot diverge.
	got := d.Key("D-im-channel", "", "1716000000.000100", "")
	want := d.UUID("D-im-channel", "1716000000.000100")
	if got != want {
		t.Fatalf("top-level key must equal UUID(channel, messageTS): %s vs %s", got, want)
	}
}

// A persona change must invalidate sessions: same (channel, thread), different
// persona → different UUID, so the old JSONL no longer matches.
func TestNamespace_PersonaChangeInvalidates(t *testing.T) {
	d1 := New(Namespace(testPrefix, "persona one"))
	d2 := New(Namespace(testPrefix, "persona two"))
	if d1.UUID("C1", "T") == d2.UUID("C1", "T") {
		t.Fatal("a persona change must produce a fresh UUID")
	}
	// Empty persona reduces to the bare prefix and is stable.
	if Namespace(testPrefix, "") != testPrefix {
		t.Fatal("empty persona should reduce to the bare prefix")
	}
}

func TestResolveFlag_ScopedToWorkspace(t *testing.T) {
	home := t.TempDir()
	ws := "/data/workspaces/C1"
	uuid := "11111111-1111-5111-8111-111111111111"

	// No JSONL yet → create-new.
	if flag, _ := ResolveFlag(home, ws, uuid); flag != "--session-id" {
		t.Fatalf("absent JSONL should be --session-id, got %s", flag)
	}

	// Write the JSONL under THIS workspace's project dir → resume.
	projDir := filepath.Join(home, ".claude", "projects", ProjectSlug(ws))
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projDir, uuid+".jsonl"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if flag, _ := ResolveFlag(home, ws, uuid); flag != "--resume" {
		t.Fatalf("present JSONL should be --resume, got %s", flag)
	}

	// A JSONL with the same UUID under a DIFFERENT workspace must NOT match.
	otherWS := "/data/workspaces/C2"
	if flag, _ := ResolveFlag(home, otherWS, uuid); flag != "--session-id" {
		t.Fatalf("a JSONL under another workspace must not be matched; got %s", flag)
	}
}

func TestProjectSlug(t *testing.T) {
	if got := ProjectSlug("/data/workspaces/C0B5W8L5744"); got != "-data-workspaces-C0B5W8L5744" {
		t.Fatalf("ProjectSlug = %q", got)
	}
}

func TestIsLockRace(t *testing.T) {
	if !IsLockRace("Error: Session ID 1234 is already in use", nil) {
		t.Fatal("should detect the lock-race message")
	}
	if IsLockRace("some other error", nil) {
		t.Fatal("should not match unrelated errors")
	}
}
