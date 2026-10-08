package test

// v1_s17: the drain degraded signal, driver/envelope edition. A
// drain-mode daemon runs on a DEDICATED machine key (a machine user's api
// key, not the bootstrap admin key), claims a wave member whose driver
// parks on an open needs_input question (the parked envelope agent), and
// then mid-run the admin revokes that key (`pf api-keys revoke`). The
// next wave fetch is authorization-denied (aihub 401), and the daemon
// must NOT silently idle and must NOT crash: a typed drain_degraded event
// lands in the durable events log (daemon scope, exactly once per
// silent→degraded episode) and is readable from the parked member's
// observe view; the process stays alive, the UDS face keeps answering,
// the mode stays drain, and the parked member is undisturbed.
import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/protocol"
	"github.com/silgrid/polyforge/internal/polyforge/observe"
)

// daemonEventCount scans the daemon's durable events log for entries of
// one kind at daemon scope (wi=""). eventsOf filters per-WI entries
// only, so the daemon-scope degraded signal needs its own scan.
func daemonEventCount(t *testing.T, d *v1Daemon, kind string) int {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(d.dataDir, "events.log"))
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatalf("events.log: %v", err)
	}
	n := 0
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e protocol.EventEntry
		if json.Unmarshal([]byte(line), &e) == nil && e.Kind == kind {
			n++
		}
	}
	return n
}

func TestV1S17DrainKeyRevokedDegradedSignal(t *testing.T) {
	inst := newV1Install(t)

	// The daemon's machine identity: a dedicated machine user + key, so
	// "admin revokes the daemon's current api-key" is semantically exact
	// (revoking the bootstrap admin key would strand the test's own
	// admin face).
	out := inst.pf(t, "users", "create", "--display-name", "s17 drain machine", "--user-type", "machine")
	// "created user <id> (machine, writer)"
	ufields := strings.Fields(out)
	if len(ufields) < 3 || ufields[0] != "created" || ufields[1] != "user" {
		t.Fatalf("users create output undecodable: %q", out)
	}
	uid := ufields[2]
	inst.pf(t, "projects", "members", "add", v1Project, "--user", uid, "--role", "writer")
	raw := inst.pf(t, "api-keys", "create", "--user", uid)
	var issued struct {
		Token struct {
			ID string `json:"id"`
		} `json:"token"`
		Plain string `json:"plain"`
	}
	if err := json.NewDecoder(strings.NewReader(raw)).Decode(&issued); err != nil {
		t.Fatalf("api-keys create output undecodable: %q (%v)", raw, err)
	}
	if issued.Token.ID == "" || issued.Plain == "" {
		t.Fatalf("api-keys create incomplete: %q", raw)
	}

	// The wave member: claimed by the drain wave; its driver parks on the
	// open question and stays there for the rest of the test.
	const qid = "q_v1s17"
	wiA := inst.newWI(t, "s17: drain wave member parks on needs_input",
		[]map[string]any{
			{"do": "envelope", "id": "grill", "outcome": "needs_input", "question_id": qid,
				"question": "s17: park here (the test never answers)."},
		}, nil)

	d := newV1Daemon(t, inst, optMode("drain"), optAPIKey(issued.Plain), optUseDriver())
	d.start()

	// The wave claims A on its own (5s wave period) and A parks. The
	// poll tolerates "not monitored yet": until the wave claims the
	// ticket the observe route answers DAEMON-404 by design.
	waitUntil(t, 60*time.Second, "wave member never parked on needs_input", func() bool {
		jv := observeSoft(d, wiA)
		return jv != nil && jv.Status == protocol.JobNeedsInput &&
			jv.Pending != nil && jv.Pending.ID == qid
	})

	// Determinism guard: the key is still valid, so no degraded signal
	// exists yet.
	if n := daemonEventCount(t, d, observe.EvDrainDegraded); n != 0 {
		t.Fatalf("pre-revoke degraded events: %d", n)
	}

	// Mid-run: the admin revokes the daemon's current api key.
	inst.pf(t, "api-keys", "revoke", issued.Token.ID)

	// The next wave fetch is authorization-denied: the typed degraded
	// event lands in the durable events log...
	waitUntil(t, 30*time.Second, "drain_degraded event never landed after revocation", func() bool {
		return daemonEventCount(t, d, observe.EvDrainDegraded) == 1
	})
	// ...exactly once per episode: two more denied waves (5s period)
	// must not re-emit.
	time.Sleep(12 * time.Second)
	if n := daemonEventCount(t, d, observe.EvDrainDegraded); n != 1 {
		t.Fatalf("degraded event count %d after multiple denied waves, want 1 (edge-triggered per episode)", n)
	}

	// The event is readable from the parked member's observe view
	// (daemon-scope entries merge into the JobView events layer) and
	// carries the pinned payload.
	var gotBody map[string]any
	for _, e := range d.observe(t, wiA).Events {
		if e.Kind != observe.EvDrainDegraded {
			continue
		}
		if e.WI != "" {
			t.Fatalf("drain_degraded must be daemon-scope (wi=\"\"), got wi=%q", e.WI)
		}
		if err := json.Unmarshal(e.Body, &gotBody); err != nil {
			t.Fatalf("drain_degraded payload undecodable: %v", err)
		}
		break
	}
	if gotBody == nil {
		t.Fatalf("drain_degraded not readable from %s's observe view", wiA)
	}
	if gotBody["project"] != v1Project || gotBody["class"] != "authorization" {
		t.Fatalf("drain_degraded payload wrong: %v", gotBody)
	}

	// 不崩: the process is alive, the UDS face still answers, the mode
	// is still drain.
	st := d.status(t)
	if st.Mode != "drain" {
		t.Fatalf("daemon left drain mode after key revocation: %q", st.Mode)
	}
	if d.exitedWithin(0) {
		t.Fatalf("daemon exited after key revocation (log tail: %s)", d.logTail())
	}

	// The parked member is undisturbed: same question on the QUESTION
	// column, still parked on the durable face.
	waitQuestion(t, d, wiA, qid, 10*time.Second)
	x := readExecution(t, d.capsuleDir(wiA))
	if x.Steps["grill"].PendingQuestion == nil || (*x.Steps["grill"].PendingQuestion)["id"] != qid {
		t.Fatalf("parked member disturbed by the revocation: %+v", x.Steps["grill"])
	}
}
