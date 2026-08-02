//go:build linux && !no_extra_providers

package main

import (
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/internal/clock"
)

// requireProcWalk skips a test on a machine whose /proc the harness cannot read.
func requireProcWalk(t *testing.T) {
	t.Helper()

	if _, ok := walkDescendants(); !ok {
		t.Skip("no readable /proc on this machine")
	}

	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh to spawn a child with")
	}
}

// spinnerScript is a POSIX-shell CPU burner. The iteration count is chosen to
// take a few hundred milliseconds on any machine this is likely to run on; the
// tests measure how long it actually took rather than assuming.
const spinnerScript = `i=0; while [ $i -lt 400000 ]; do i=$((i+1)); done`

func TestParseProcStat(t *testing.T) {
	// A comm field with a space, a parenthesis and a digit in it - the reason
	// this file is parsed from the LAST ')' and not by splitting on spaces.
	line := "1234 (we (ird) name) S 1000 1234 1234 0 -1 4194304 900 0 0 0 " +
		// utime=250 stime=125 (fields 14 and 15)
		"250 125 3 4 20 0 8 0 " +
		// starttime (field 22)
		"987654 " +
		"12345678 900 18446744073709551615 1 2 3\n"

	st, ok := parseProcStat(line)
	require.True(t, ok)
	require.Equal(t, 1000, st.ppid)
	require.Equal(t, procKey{pid: 1234, start: 987654}, st.key)
	require.Equal(t, 2500*time.Millisecond, st.cpu.user, "250 ticks at 100 Hz")
	require.Equal(t, 1250*time.Millisecond, st.cpu.sys)
}

func TestParseProcStatRejectsGarbage(t *testing.T) {
	for _, line := range []string{"", "no parenthesis here", "1 (sh) S", "(sh) S 1 2 3"} {
		_, ok := parseProcStat(line)
		require.False(t, ok, "line %q", line)
	}
}

func TestIsDescendantOf(t *testing.T) {
	all := map[int]procStat{
		10: {ppid: 1},
		11: {ppid: 10},
		12: {ppid: 11},
		20: {ppid: 1},
		// A cycle, which only a racy /proc read could produce: it must terminate.
		30: {ppid: 31},
		31: {ppid: 30},
	}

	require.True(t, isDescendantOf(all, all[11], 10))
	require.True(t, isDescendantOf(all, all[12], 10), "grandchildren count")
	require.False(t, isDescendantOf(all, all[20], 10))
	require.False(t, isDescendantOf(all, all[30], 10))
}

func TestWalkDescendantsSeesAChild(t *testing.T) {
	requireProcWalk(t)

	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", "sleep 5")
	require.NoError(t, cmd.Start())

	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	live, ok := walkDescendants()
	require.True(t, ok)

	found := false

	for k := range live {
		if k.pid == cmd.Process.Pid {
			found = true
		}
	}

	require.True(t, found, "the walk must find a live child of this process")
}

// TestDescendantCPUGroundTruth is the ground-truth check: a child that burns a
// known amount of CPU - one core, saturated, for as long as it takes - must be
// attributed roughly that amount, and not twice it.
//
// It covers three things at once:
//
//   - the exit-mid-phase catch-net: the child starts and finishes inside the
//     measured window, so its CPU is only there at all because the poller saw it
//     before it vanished;
//   - grandchildren: the spinner is an inner shell, the shape of kopia spawning
//     rclone, so a walk that only looked at direct children would see nothing;
//   - reconciliation: the child is reaped inside the window, so its whole life
//     also lands in RUSAGE_CHILDREN. Double counting would show up as ~2x wall.
func TestDescendantCPUGroundTruth(t *testing.T) {
	requireProcWalk(t)

	m := startCPUMeterOn(newCPULedger())

	// The trailing ':' keeps the outer shell from exec'ing the inner one, which
	// would collapse the grandchild into the child.
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", "sh -c '"+spinnerScript+"' ; :")
	start := clock.Now()

	require.NoError(t, cmd.Run())

	wall := clock.Now().Sub(start)

	s := m.stop().withProcessState(cmd.ProcessState)

	require.Greater(t, wall, 100*time.Millisecond, "the spinner finished too fast to measure")
	require.True(t, s.fromProc)
	require.True(t, s.fromRusage)

	// The spinner is single-threaded and CPU-bound, so its CPU is its wall clock.
	require.Greater(t, s.children.total(), wall/2,
		"a saturated child burned %v of wall clock but only %v was attributed", wall, s.children.total())
	// A single-threaded spinner cannot burn more CPU than the wall clock it ran
	// for, so anything above that is the live attribution and the reap lump both
	// being charged. The margin is one poll interval's worth of slack, not room
	// for a second copy of the measurement.
	require.Less(t, s.children.total(), wall*11/10+100*time.Millisecond,
		"attributed %v for %v of single-core work: the live attribution and the reap lump were both charged", s.children.total(), wall)
}

// TestDescendantCPUIsAttributedPerPhase is the requirement the rclone lane
// depends on: kopia's `rclone serve webdav` outlives every individual phase, so
// each phase must be charged what that process burned INSIDE it - not zero
// (which is what reap-time accounting would give) and not its whole life (which
// is what a naive cumulative read would give).
func TestDescendantCPUIsAttributedPerPhase(t *testing.T) {
	requireProcWalk(t)

	ledger := newCPULedger()

	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", "while : ; do : ; done")
	require.NoError(t, cmd.Start())

	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	// Burned before any phase opens, and therefore owed to no phase.
	time.Sleep(600 * time.Millisecond)

	const phase = 700 * time.Millisecond

	for i, s := range []cpuSample{measurePhase(ledger, phase), measurePhase(ledger, phase)} {
		require.True(t, s.fromProc, "phase %v", i)
		require.Greater(t, s.children.total(), phase/2,
			"phase %v saw only %v of a child that was saturating a core throughout it", i, s.children.total())
		require.Less(t, s.children.total(), phase*3/2,
			"phase %v was charged %v, which is more than it lasted: CPU from outside the window leaked in", i, s.children.total())
	}
}

// measurePhase opens a window, waits, and closes it.
func measurePhase(l *cpuLedger, d time.Duration) cpuSample {
	m := startCPUMeterOn(l)

	time.Sleep(d)

	return m.stop()
}

// TestTrialCPUMeterCoversUnattributedWork pins the residual: work done outside
// every measured phase still shows up in the trial total, which is what makes
// cpu_residual_ms an audit of the attribution.
func TestTrialCPUMeterCoversUnattributedWork(t *testing.T) {
	requireProcWalk(t)

	trial := startTrialCPUMeter()

	// Unattributed: no phase is open. This is what a workload's setup writes and
	// cleanup deletes look like to the accounting.
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", spinnerScript)
	require.NoError(t, cmd.Run())

	attributed := measurePhase(newCPULedger(), 100*time.Millisecond)

	total, ok := trial.stop()
	require.True(t, ok)

	require.Greater(t, total.total(), 100*time.Millisecond, "the trial total must include work no phase claimed")
	require.Greater(t, total.total(), attributed.total().total(), "the residual must be positive here")
}
