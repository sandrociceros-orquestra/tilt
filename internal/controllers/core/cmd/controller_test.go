package cmd

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/pointer"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/tilt-dev/tilt/internal/controllers/fake"
	"github.com/tilt-dev/tilt/internal/engine/local"
	"github.com/tilt-dev/tilt/internal/store"
	"github.com/tilt-dev/tilt/internal/testutils/configmap"
	"github.com/tilt-dev/tilt/pkg/apis"
	"github.com/tilt-dev/tilt/pkg/apis/core/v1alpha1"
	"github.com/tilt-dev/tilt/pkg/model"
)

var timeout = time.Second
var interval = 5 * time.Millisecond

func TestNoop(t *testing.T) {
	f := newFixture(t)

	f.step()
	f.assertCmdCount(0)
}

func TestUpdate(t *testing.T) {
	f := newFixture(t)

	t1 := time.Unix(1, 0)
	f.resource("foo", "true", ".", t1)
	f.step()
	f.assertCmdMatches("foo-serve-1", func(cmd *Cmd) bool {
		return cmd.Status.Running != nil
	})

	t2 := time.Unix(2, 0)
	f.resource("foo", "false", ".", t2)
	f.step()
	f.assertCmdDeleted("foo-serve-1")

	f.step()
	f.assertCmdMatches("foo-serve-2", func(cmd *Cmd) bool {
		return cmd.Status.Running != nil
	})

	f.fe.RequireNoKnownProcess(t, "true")
	f.assertLogMessage("foo", "Starting cmd false")
	f.assertLogMessage("foo", "cmd true canceled")
	f.assertCmdCount(1)
}

func TestUpdateWithCurrentBuild(t *testing.T) {
	f := newFixture(t)

	t1 := time.Unix(1, 0)
	f.resource("foo", "true", ".", t1)
	f.step()
	f.assertCmdMatches("foo-serve-1", func(cmd *Cmd) bool {
		return cmd.Status.Running != nil
	})

	f.st.WithState(func(s *store.EngineState) {
		c := model.ToHostCmd("false")
		localTarget := model.NewLocalTarget(model.TargetName("foo"), c, c, nil)
		s.ManifestTargets["foo"].Manifest.DeployTarget = localTarget
		s.ManifestTargets["foo"].State.CurrentBuilds["buildcontrol"] = model.BuildRecord{StartTime: f.clock.Now()}
	})

	f.step()

	assert.Never(t, func() bool {
		return f.st.Cmd("foo-serve-2") != nil
	}, 20*time.Millisecond, 5*time.Millisecond)

	f.st.WithState(func(s *store.EngineState) {
		delete(s.ManifestTargets["foo"].State.CurrentBuilds, "buildcontrol")
	})

	f.step()
	f.assertCmdDeleted("foo-serve-1")
}

func TestServe(t *testing.T) {
	f := newFixture(t)

	t1 := time.Unix(1, 0)
	f.resource("foo", "sleep 60", "testdir", t1)
	f.step()
	f.assertCmdMatches("foo-serve-1", func(cmd *Cmd) bool {
		return cmd.Status.Running != nil && cmd.Status.Ready
	})

	require.Equal(t, "testdir", f.fe.processes["sleep 60"].workdir)

	f.assertLogMessage("foo", "Starting cmd sleep 60")
}

func TestServeReadinessProbe(t *testing.T) {
	f := newFixture(t)

	t1 := time.Unix(1, 0)

	c := model.ToHostCmdInDir("sleep 60", "testdir")
	localTarget := model.NewLocalTarget("foo", model.Cmd{}, c, nil)
	localTarget.ReadinessProbe = &v1alpha1.Probe{
		TimeoutSeconds: 5,
		Handler: v1alpha1.Handler{
			Exec: &v1alpha1.ExecAction{Command: []string{"sleep", "15"}},
		},
	}

	f.resourceFromTarget("foo", localTarget, t1)
	f.step()
	f.assertCmdMatches("foo-serve-1", func(cmd *Cmd) bool {
		return cmd.Status.Running != nil && cmd.Status.Ready
	})
	f.assertLogMessage("foo", "[readiness probe: success] fake probe succeeded")

	assert.Equal(t, "sleep", f.fpm.execName)
	assert.Equal(t, []string{"15"}, f.fpm.execArgs)
	assert.GreaterOrEqual(t, f.fpm.ProbeCount(), 1)
}

func TestServeReadinessProbeInvalidSpec(t *testing.T) {
	f := newFixture(t)

	t1 := time.Unix(1, 0)

	c := model.ToHostCmdInDir("sleep 60", "testdir")
	localTarget := model.NewLocalTarget("foo", model.Cmd{}, c, nil)
	localTarget.ReadinessProbe = &v1alpha1.Probe{
		Handler: v1alpha1.Handler{HTTPGet: &v1alpha1.HTTPGetAction{
			// port > 65535
			Port: 70000,
		}},
	}

	f.resourceFromTarget("foo", localTarget, t1)
	f.step()

	f.assertCmdMatches("foo-serve-1", func(cmd *Cmd) bool {
		return cmd.Status.Terminated != nil && cmd.Status.Terminated.ExitCode == 1
	})

	f.assertLogMessage("foo", "Invalid readiness probe: port number out of range: 70000")
	assert.Equal(t, 0, f.fpm.ProbeCount())
}

func TestFailure(t *testing.T) {
	f := newFixture(t)

	t1 := time.Unix(1, 0)
	f.resource("foo", "true", ".", t1)
	f.step()
	f.assertCmdMatches("foo-serve-1", func(cmd *Cmd) bool {
		return cmd.Status.Running != nil
	})

	f.assertLogMessage("foo", "Starting cmd true")

	err := f.fe.stop("true", 5)
	require.NoError(t, err)
	f.assertCmdMatches("foo-serve-1", func(cmd *Cmd) bool {
		return cmd.Status.Terminated != nil && cmd.Status.Terminated.ExitCode == 5
	})

	f.assertLogMessage("foo", "cmd true exited with code 5")
}

func TestUniqueSpanIDs(t *testing.T) {
	f := newFixture(t)

	t1 := time.Unix(1, 0)
	f.resource("foo", "foo.sh", ".", t1)
	f.resource("bar", "bar.sh", ".", t1)
	f.step()

	fooStart := f.waitForLogEventContaining("Starting cmd foo.sh")
	barStart := f.waitForLogEventContaining("Starting cmd bar.sh")
	require.NotEqual(t, fooStart.SpanID(), barStart.SpanID(), "different resources should have unique log span ids")
}

func TestTearDown(t *testing.T) {
	f := newFixture(t)

	t1 := time.Unix(1, 0)
	f.resource("foo", "foo.sh", ".", t1)
	f.resource("bar", "bar.sh", ".", t1)
	f.step()

	f.c.TearDown(f.Context())

	f.fe.RequireNoKnownProcess(t, "foo.sh")
	f.fe.RequireNoKnownProcess(t, "bar.sh")
}

func TestRestartOnFileWatch(t *testing.T) {
	f := newFixture(t)

	f.resource("cmd", "true", ".", f.clock.Now())
	f.step()

	firstStart := f.assertCmdMatches("cmd-serve-1", func(cmd *Cmd) bool {
		return cmd.Status.Running != nil
	})

	fw := &FileWatch{
		ObjectMeta: ObjectMeta{
			Name: "fw-1",
		},
		Spec: FileWatchSpec{
			WatchedPaths: []string{t.TempDir()},
		},
	}
	err := f.Client.Create(f.Context(), fw)
	require.NoError(t, err)

	f.clock.Advance(time.Second)
	f.updateSpec("cmd-serve-1", func(spec *v1alpha1.CmdSpec) {
		spec.RestartOn = &RestartOnSpec{
			FileWatches: []string{"fw-1"},
		}
	})

	f.clock.Advance(time.Second)
	f.triggerFileWatch("fw-1")
	f.reconcileCmd("cmd-serve-1")

	f.assertCmdMatches("cmd-serve-1", func(cmd *Cmd) bool {
		running := cmd.Status.Running
		return running != nil && running.StartedAt.Time.After(firstStart.Status.Running.StartedAt.Time)
	})

	// Our fixture doesn't test reconcile.Request triage,
	// so test it manually here.
	assert.Equal(f.T(),
		[]reconcile.Request{
			reconcile.Request{NamespacedName: types.NamespacedName{Name: "cmd-serve-1"}},
		},
		f.c.indexer.Enqueue(context.Background(), fw))
}

func TestRestartOnUIButton(t *testing.T) {
	f := newFixture(t)

	f.resource("cmd", "true", ".", f.clock.Now())
	f.step()

	firstStart := f.assertCmdMatches("cmd-serve-1", func(cmd *Cmd) bool {
		return cmd.Status.Running != nil
	})

	f.clock.Advance(time.Second)
	f.updateSpec("cmd-serve-1", func(spec *v1alpha1.CmdSpec) {
		spec.RestartOn = &RestartOnSpec{
			UIButtons: []string{"b-1"},
		}
	})

	b := &UIButton{
		ObjectMeta: ObjectMeta{
			Name: "b-1",
		},
		Spec: UIButtonSpec{},
	}
	err := f.Client.Create(f.Context(), b)
	require.NoError(t, err)

	f.clock.Advance(time.Second)
	f.triggerButton("b-1", f.clock.Now())
	f.reconcileCmd("cmd-serve-1")

	f.assertCmdMatches("cmd-serve-1", func(cmd *Cmd) bool {
		running := cmd.Status.Running
		return running != nil && running.StartedAt.Time.After(firstStart.Status.Running.StartedAt.Time)
	})

	// Our fixture doesn't test reconcile.Request triage,
	// so test it manually here.
	assert.Equal(f.T(),
		[]reconcile.Request{
			reconcile.Request{NamespacedName: types.NamespacedName{Name: "cmd-serve-1"}},
		},
		f.c.indexer.Enqueue(context.Background(), b))
}

func setupStartOnTest(t *testing.T, f *fixture) {
	cmd := &Cmd{
		ObjectMeta: metav1.ObjectMeta{
			Name: "testcmd",
		},
		Spec: v1alpha1.CmdSpec{
			Args: []string{"myserver"},
			StartOn: &StartOnSpec{
				UIButtons:  []string{"b-1"},
				StartAfter: apis.NewTime(f.clock.Now()),
			},
		},
	}

	err := f.Client.Create(f.Context(), cmd)
	require.NoError(t, err)

	b := &UIButton{
		ObjectMeta: ObjectMeta{
			Name: "b-1",
		},
		Spec: UIButtonSpec{},
	}
	err = f.Client.Create(f.Context(), b)
	require.NoError(t, err)

	f.reconcileCmd("testcmd")

	f.fe.RequireNoKnownProcess(t, "myserver")
}

func TestStartOnNoPreviousProcess(t *testing.T) {
	f := newFixture(t)

	startup := f.clock.Now()

	setupStartOnTest(t, f)

	f.clock.Advance(time.Second)

	f.triggerButton("b-1", f.clock.Now())
	f.reconcileCmd("testcmd")

	f.requireCmdMatchesInAPI("testcmd", func(cmd *Cmd) bool {
		running := cmd.Status.Running
		return running != nil && running.StartedAt.Time.After(startup)
	})
}

func TestStartOnDoesntRunOnCreation(t *testing.T) {
	f := newFixture(t)

	setupStartOnTest(t, f)

	f.reconcileCmd("testcmd")

	f.requireCmdMatchesInAPI("testcmd", func(cmd *Cmd) bool {
		return cmd.Status.Waiting != nil && cmd.Status.Waiting.Reason == waitingOnStartOnReason
	})

	f.fe.RequireNoKnownProcess(t, "myserver")
}

func TestStartOnStartAfter(t *testing.T) {
	f := newFixture(t)

	setupStartOnTest(t, f)

	f.triggerButton("b-1", f.clock.Now().Add(-time.Minute))

	f.reconcileCmd("testcmd")

	f.requireCmdMatchesInAPI("testcmd", func(cmd *Cmd) bool {
		return cmd.Status.Waiting != nil && cmd.Status.Waiting.Reason == waitingOnStartOnReason
	})

	f.fe.RequireNoKnownProcess(t, "myserver")
}

func TestStartOnRunningProcess(t *testing.T) {
	f := newFixture(t)

	setupStartOnTest(t, f)

	f.clock.Advance(time.Second)
	f.triggerButton("b-1", f.clock.Now())
	f.reconcileCmd("testcmd")

	// wait for the initial process to start
	f.requireCmdMatchesInAPI("testcmd", func(cmd *Cmd) bool {
		return cmd.Status.Running != nil
	})

	f.fe.mu.Lock()
	st := f.fe.processes["myserver"].startTime
	f.fe.mu.Unlock()

	f.clock.Advance(time.Second)

	secondClickTime := f.clock.Now()
	f.triggerButton("b-1", secondClickTime)
	f.reconcileCmd("testcmd")

	f.requireCmdMatchesInAPI("testcmd", func(cmd *Cmd) bool {
		running := cmd.Status.Running
		return running != nil && !running.StartedAt.Time.Before(secondClickTime)
	})

	// make sure it's not the same process
	f.fe.mu.Lock()
	p, ok := f.fe.processes["myserver"]
	require.True(t, ok)
	require.NotEqual(t, st, p.startTime)
	f.fe.mu.Unlock()
}

func TestStartOnPreviousTerminatedProcess(t *testing.T) {
	f := newFixture(t)

	firstClickTime := f.clock.Now()

	setupStartOnTest(t, f)

	f.triggerButton("b-1", firstClickTime)
	f.reconcileCmd("testcmd")

	// wait for the initial process to start
	f.requireCmdMatchesInAPI("testcmd", func(cmd *Cmd) bool {
		return cmd.Status.Running != nil
	})

	f.fe.mu.Lock()
	st := f.fe.processes["myserver"].startTime
	f.fe.mu.Unlock()

	err := f.fe.stop("myserver", 1)
	require.NoError(t, err)

	// wait for the initial process to die
	f.requireCmdMatchesInAPI("testcmd", func(cmd *Cmd) bool {
		return cmd.Status.Terminated != nil
	})

	f.clock.Advance(time.Second)
	secondClickTime := f.clock.Now()
	f.triggerButton("b-1", secondClickTime)
	f.reconcileCmd("testcmd")

	f.requireCmdMatchesInAPI("testcmd", func(cmd *Cmd) bool {
		running := cmd.Status.Running
		return running != nil && !running.StartedAt.Time.Before(secondClickTime)
	})

	// make sure it's not the same process
	f.fe.mu.Lock()
	p, ok := f.fe.processes["myserver"]
	require.True(t, ok)
	require.NotEqual(t, st, p.startTime)
	f.fe.mu.Unlock()
}

func TestDisposeOrphans(t *testing.T) {
	f := newFixture(t)

	t1 := time.Unix(1, 0)
	f.resource("foo", "true", ".", t1)
	f.step()
	f.assertCmdMatches("foo-serve-1", func(cmd *Cmd) bool {
		return cmd.Status.Running != nil
	})

	f.st.WithState(func(es *store.EngineState) {
		es.RemoveManifestTarget("foo")
	})
	f.step()
	f.assertCmdCount(0)
	f.fe.RequireNoKnownProcess(t, "true")
}

func TestDisposeTerminatedWhenCmdChanges(t *testing.T) {
	f := newFixture(t)

	t1 := time.Unix(1, 0)
	f.resource("foo", "true", ".", t1)
	f.step()

	f.assertCmdMatches("foo-serve-1", func(cmd *Cmd) bool {
		return cmd.Status.Running != nil
	})

	err := f.fe.stop("true", 0)
	require.NoError(t, err)

	f.assertCmdMatches("foo-serve-1", func(cmd *Cmd) bool {
		return cmd.Status.Terminated != nil
	})

	f.resource("foo", "true", "subdir", t1)
	f.step()
	f.assertCmdMatches("foo-serve-2", func(cmd *Cmd) bool {
		return cmd.Status.Running != nil
	})
	f.assertCmdDeleted("foo-serve-1")
}

func TestDisableCmd(t *testing.T) {
	f := newFixture(t)

	cmd := &Cmd{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cmd-1",
		},
		Spec: v1alpha1.CmdSpec{
			Args: []string{"sh", "-c", "sleep 10000"},
			DisableSource: &v1alpha1.DisableSource{
				ConfigMap: &v1alpha1.ConfigMapDisableSource{
					Name: "disable-cmd-1",
					Key:  "isDisabled",
				},
			},
		},
	}
	err := f.Client.Create(f.Context(), cmd)
	require.NoError(t, err)

	f.setDisabled(cmd.Name, false)

	f.requireCmdMatchesInAPI(cmd.Name, func(cmd *Cmd) bool {
		return cmd.Status.Running != nil &&
			cmd.Status.DisableStatus != nil &&
			cmd.Status.DisableStatus.State == v1alpha1.DisableStateEnabled
	})

	f.setDisabled(cmd.Name, true)

	f.requireCmdMatchesInAPI(cmd.Name, func(cmd *Cmd) bool {
		return cmd.Status.Terminated != nil &&
			cmd.Status.DisableStatus != nil &&
			cmd.Status.DisableStatus.State == v1alpha1.DisableStateDisabled
	})

	f.setDisabled(cmd.Name, false)

	f.requireCmdMatchesInAPI(cmd.Name, func(cmd *Cmd) bool {
		return cmd.Status.Running != nil &&
			cmd.Status.DisableStatus != nil &&
			cmd.Status.DisableStatus.State == v1alpha1.DisableStateEnabled
	})
}

func TestReenable(t *testing.T) {
	f := newFixture(t)

	cmd := &Cmd{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cmd-1",
		},
		Spec: v1alpha1.CmdSpec{
			Args: []string{"sh", "-c", "sleep 10000"},
			DisableSource: &v1alpha1.DisableSource{
				ConfigMap: &v1alpha1.ConfigMapDisableSource{
					Name: "disable-cmd-1",
					Key:  "isDisabled",
				},
			},
		},
	}
	err := f.Client.Create(f.Context(), cmd)
	require.NoError(t, err)

	f.setDisabled(cmd.Name, true)

	f.requireCmdMatchesInAPI(cmd.Name, func(cmd *Cmd) bool {
		return cmd.Status.Running == nil &&
			cmd.Status.DisableStatus != nil &&
			cmd.Status.DisableStatus.State == v1alpha1.DisableStateDisabled
	})

	f.setDisabled(cmd.Name, false)

	f.requireCmdMatchesInAPI(cmd.Name, func(cmd *Cmd) bool {
		return cmd.Status.Running != nil &&
			cmd.Status.DisableStatus != nil &&
			cmd.Status.DisableStatus.State == v1alpha1.DisableStateEnabled
	})
}

func TestDisableServeCmd(t *testing.T) {
	f := newFixture(t)

	ds := v1alpha1.DisableSource{ConfigMap: &v1alpha1.ConfigMapDisableSource{Name: "disable-foo", Key: "isDisabled"}}
	t1 := time.Unix(1, 0)
	localTarget := model.NewLocalTarget("foo", model.Cmd{}, model.ToHostCmd("."), nil)
	localTarget.ServeCmdDisableSource = &ds
	err := configmap.UpsertDisableConfigMap(f.Context(), f.Client, ds.ConfigMap.Name, ds.ConfigMap.Key, false)
	require.NoError(t, err)

	f.resourceFromTarget("foo", localTarget, t1)

	f.step()
	f.requireCmdMatchesInAPI("foo-serve-1", func(cmd *Cmd) bool {
		return cmd != nil && cmd.Status.Running != nil
	})

	err = configmap.UpsertDisableConfigMap(f.Context(), f.Client, ds.ConfigMap.Name, ds.ConfigMap.Key, true)
	require.NoError(t, err)

	f.step()
	f.assertCmdCount(0)
}

func TestEnableServeCmd(t *testing.T) {
	f := newFixture(t)

	ds := v1alpha1.DisableSource{ConfigMap: &v1alpha1.ConfigMapDisableSource{Name: "disable-foo", Key: "isDisabled"}}
	err := configmap.UpsertDisableConfigMap(f.Context(), f.Client, ds.ConfigMap.Name, ds.ConfigMap.Key, true)
	require.NoError(t, err)

	t1 := time.Unix(1, 0)
	localTarget := model.NewLocalTarget("foo", model.Cmd{}, model.ToHostCmd("."), nil)
	localTarget.ServeCmdDisableSource = &ds
	f.resourceFromTarget("foo", localTarget, t1)

	f.step()
	f.assertCmdCount(0)
	err = configmap.UpsertDisableConfigMap(f.Context(), f.Client, ds.ConfigMap.Name, ds.ConfigMap.Key, false)
	require.NoError(t, err)

	f.step()
	f.requireCmdMatchesInAPI("foo-serve-1", func(cmd *Cmd) bool {
		return cmd != nil && cmd.Status.Running != nil
	})
}

// Self-modifying Cmds are typically paired with a StartOn trigger,
// to simulate a "toggle" switch on the Cmd.
//
// See:
// https://github.com/tilt-dev/tilt-extensions/issues/202
func TestSelfModifyingCmd(t *testing.T) {
	f := newFixture(t)

	setupStartOnTest(t, f)

	f.reconcileCmd("testcmd")

	f.requireCmdMatchesInAPI("testcmd", func(cmd *Cmd) bool {
		return cmd.Status.Waiting != nil && cmd.Status.Waiting.Reason == waitingOnStartOnReason
	})

	f.clock.Advance(time.Second)
	f.triggerButton("b-1", f.clock.Now())
	f.clock.Advance(time.Second)
	f.reconcileCmd("testcmd")

	f.requireCmdMatchesInAPI("testcmd", func(cmd *Cmd) bool {
		return cmd.Status.Running != nil
	})

	f.updateSpec("testcmd", func(spec *v1alpha1.CmdSpec) {
		spec.Args = []string{"yourserver"}
	})
	f.reconcileCmd("testcmd")
	f.requireCmdMatchesInAPI("testcmd", func(cmd *Cmd) bool {
		return cmd.Status.Waiting != nil && cmd.Status.Waiting.Reason == waitingOnStartOnReason
	})

	f.fe.RequireNoKnownProcess(t, "myserver")
	f.fe.RequireNoKnownProcess(t, "yourserver")
	f.clock.Advance(time.Second)
	f.triggerButton("b-1", f.clock.Now())
	f.reconcileCmd("testcmd")

	f.requireCmdMatchesInAPI("testcmd", func(cmd *Cmd) bool {
		return cmd.Status.Running != nil
	})
}

// Ensure that changes to the StartOn or RestartOn fields
// don't restart the command.
func TestDependencyChangesDoNotCauseRestart(t *testing.T) {
	f := newFixture(t)

	setupStartOnTest(t, f)
	f.triggerButton("b-1", f.clock.Now())
	f.clock.Advance(time.Second)
	f.reconcileCmd("testcmd")

	firstStart := f.requireCmdMatchesInAPI("testcmd", func(cmd *Cmd) bool {
		return cmd.Status.Running != nil
	})

	err := f.Client.Create(f.Context(), &v1alpha1.UIButton{ObjectMeta: metav1.ObjectMeta{Name: "new-button"}})
	require.NoError(t, err)

	err = f.Client.Create(f.Context(), &v1alpha1.FileWatch{
		ObjectMeta: metav1.ObjectMeta{Name: "new-filewatch"},
		Spec: FileWatchSpec{
			WatchedPaths: []string{t.TempDir()},
		},
	})
	require.NoError(t, err)

	f.updateSpec("testcmd", func(spec *v1alpha1.CmdSpec) {
		spec.StartOn = &v1alpha1.StartOnSpec{
			UIButtons: []string{"new-button"},
		}
		spec.RestartOn = &v1alpha1.RestartOnSpec{
			FileWatches: []string{"new-filewatch"},
		}
	})
	f.reconcileCmd("testcmd")

	f.requireCmdMatchesInAPI("testcmd", func(cmd *Cmd) bool {
		running := cmd.Status.Running
		return running != nil && running.StartedAt.Time.Equal(firstStart.Status.Running.StartedAt.Time)
	})
}

func TestCmdUsesInputsFromButtonOnStart(t *testing.T) {
	f := newFixture(t)

	setupStartOnTest(t, f)
	f.updateButton("b-1", func(button *v1alpha1.UIButton) {
		button.Spec.Inputs = []v1alpha1.UIInputSpec{
			{Name: "foo", Text: &v1alpha1.UITextInputSpec{}},
			{Name: "baz", Text: &v1alpha1.UITextInputSpec{}},
		}
		button.Status.Inputs = []v1alpha1.UIInputStatus{
			{
				Name: "foo",
				Text: &v1alpha1.UITextInputStatus{Value: "bar"},
			},
			{
				Name: "baz",
				Text: &v1alpha1.UITextInputStatus{Value: "wait what comes next"},
			},
		}
	})
	f.triggerButton("b-1", f.clock.Now())
	f.reconcileCmd("testcmd")

	actualEnv := f.fe.processes["myserver"].env
	expectedEnv := []string{"foo=bar", "baz=wait what comes next"}
	require.Equal(t, expectedEnv, actualEnv)
}

func TestBoolInput(t *testing.T) {
	for _, tc := range []struct {
		name          string
		input         v1alpha1.UIBoolInputSpec
		value         bool
		expectedValue string
	}{
		{"true, default", v1alpha1.UIBoolInputSpec{}, true, "true"},
		{"true, custom", v1alpha1.UIBoolInputSpec{TrueString: pointer.String("custom value")}, true, "custom value"},
		{"false, default", v1alpha1.UIBoolInputSpec{}, false, "false"},
		{"false, custom", v1alpha1.UIBoolInputSpec{FalseString: pointer.String("ooh la la")}, false, "ooh la la"},
		{"false, empty", v1alpha1.UIBoolInputSpec{FalseString: pointer.String("")}, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)

			setupStartOnTest(t, f)
			f.updateButton("b-1", func(button *v1alpha1.UIButton) {
				spec := v1alpha1.UIInputSpec{Name: "dry_run", Bool: &tc.input}
				button.Spec.Inputs = append(button.Spec.Inputs, spec)
				status := v1alpha1.UIInputStatus{Name: "dry_run", Bool: &v1alpha1.UIBoolInputStatus{Value: tc.value}}
				button.Status.Inputs = append(button.Status.Inputs, status)
			})
			f.triggerButton("b-1", f.clock.Now())
			f.reconcileCmd("testcmd")

			actualEnv := f.fe.processes["myserver"].env
			expectedEnv := []string{fmt.Sprintf("dry_run=%s", tc.expectedValue)}
			require.Equal(t, expectedEnv, actualEnv)
		})
	}
}

func TestHiddenInput(t *testing.T) {
	f := newFixture(t)

	val := "afds"

	setupStartOnTest(t, f)
	f.updateButton("b-1", func(button *v1alpha1.UIButton) {
		spec := v1alpha1.UIInputSpec{Name: "foo", Hidden: &v1alpha1.UIHiddenInputSpec{Value: val}}
		button.Spec.Inputs = append(button.Spec.Inputs, spec)
		status := v1alpha1.UIInputStatus{Name: "foo", Hidden: &v1alpha1.UIHiddenInputStatus{Value: val}}
		button.Status.Inputs = append(button.Status.Inputs, status)
	})
	f.triggerButton("b-1", f.clock.Now())
	f.reconcileCmd("testcmd")

	actualEnv := f.fe.processes["myserver"].env
	expectedEnv := []string{fmt.Sprintf("foo=%s", val)}
	require.Equal(t, expectedEnv, actualEnv)
}

func TestChoiceInput(t *testing.T) {
	for _, tc := range []struct {
		name          string
		input         v1alpha1.UIChoiceInputSpec
		value         string
		expectedValue string
	}{
		{"empty value", v1alpha1.UIChoiceInputSpec{Choices: []string{"choice1", "choice2"}}, "", "choice1"},
		{"invalid value", v1alpha1.UIChoiceInputSpec{Choices: []string{"choice1", "choice2"}}, "not in Choices", "choice1"},
		{"selected choice1", v1alpha1.UIChoiceInputSpec{Choices: []string{"choice1", "choice2"}}, "choice1", "choice1"},
		{"selected choice2", v1alpha1.UIChoiceInputSpec{Choices: []string{"choice1", "choice2"}}, "choice2", "choice2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)

			setupStartOnTest(t, f)
			f.updateButton("b-1", func(button *v1alpha1.UIButton) {
				spec := v1alpha1.UIInputSpec{Name: "dry_run", Choice: &tc.input}
				button.Spec.Inputs = append(button.Spec.Inputs, spec)
				status := v1alpha1.UIInputStatus{Name: "dry_run", Choice: &v1alpha1.UIChoiceInputStatus{Value: tc.value}}
				button.Status.Inputs = append(button.Status.Inputs, status)
			})
			f.triggerButton("b-1", f.clock.Now())
			f.reconcileCmd("testcmd")

			actualEnv := f.fe.processes["myserver"].env
			expectedEnv := []string{fmt.Sprintf("dry_run=%s", tc.expectedValue)}
			require.Equal(t, expectedEnv, actualEnv)
		})
	}
}

func TestCmdOnlyUsesButtonThatStartedIt(t *testing.T) {
	f := newFixture(t)

	setupStartOnTest(t, f)
	f.updateButton("b-1", func(button *v1alpha1.UIButton) {
		inputs := []v1alpha1.UIInputStatus{
			{
				Name: "foo",
				Text: &v1alpha1.UITextInputStatus{Value: "bar"},
			},
			{
				Name: "baz",
				Text: &v1alpha1.UITextInputStatus{Value: "wait what comes next"},
			},
		}
		button.Status.Inputs = append(button.Status.Inputs, inputs...)
	})

	b := &UIButton{
		ObjectMeta: ObjectMeta{
			Name: "b-2",
		},
		Spec: UIButtonSpec{},
	}
	err := f.Client.Create(f.Context(), b)
	require.NoError(t, err)
	f.updateSpec("testcmd", func(spec *v1alpha1.CmdSpec) {
		spec.StartOn.UIButtons = append(spec.StartOn.UIButtons, "b-2")
	})
	f.triggerButton("b-2", f.clock.Now())
	f.reconcileCmd("testcmd")

	actualEnv := f.fe.processes["myserver"].env
	// b-1's env gets ignored since it was triggered by b-2
	expectedEnv := []string{}
	require.Equal(t, expectedEnv, actualEnv)
}

type testStore struct {
	*store.TestingStore
	out     io.Writer
	summary store.ChangeSummary
}

func NewTestingStore(out io.Writer) *testStore {
	return &testStore{
		TestingStore: store.NewTestingStore(),
		out:          out,
	}
}

func (s *testStore) Cmd(name string) *Cmd {
	st := s.RLockState()
	defer s.RUnlockState()
	return st.Cmds[name]
}

func (s *testStore) CmdCount() int {
	st := s.RLockState()
	defer s.RUnlockState()
	count := 0
	for _, cmd := range st.Cmds {
		if cmd.DeletionTimestamp == nil {
			count++
		}
	}
	return count
}

func (s *testStore) Dispatch(action store.Action) {
	s.TestingStore.Dispatch(action)

	st := s.LockMutableStateForTesting()
	defer s.UnlockMutableState()

	switch action := action.(type) {
	case store.ErrorAction:
		panic(fmt.Sprintf("no error action allowed: %s", action.Error))

	case store.LogAction:
		_, _ = s.out.Write(action.Message())

	case local.CmdCreateAction:
		local.HandleCmdCreateAction(st, action)
		action.Summarize(&s.summary)

	case local.CmdUpdateStatusAction:
		local.HandleCmdUpdateStatusAction(st, action)

	case local.CmdDeleteAction:
		local.HandleCmdDeleteAction(st, action)
		action.Summarize(&s.summary)
	}
}

type fixture struct {
	*fake.ControllerFixture
	st    *testStore
	fe    *FakeExecer
	fpm   *FakeProberManager
	sc    *local.ServerController
	c     *Controller
	clock clockwork.FakeClock
}

func newFixture(t *testing.T) *fixture {
	f := fake.NewControllerFixtureBuilder(t)
	st := NewTestingStore(f.OutWriter())

	fe := NewFakeExecer()
	fpm := NewFakeProberManager()
	sc := local.NewServerController(f.Client)

	// Fake clock is set to 2006-01-02 15:04:05
	// This helps ensure that nanosecond rounding in time doesn't break tests.
	clock := clockwork.NewFakeClockAt(time.Date(2006, time.January, 2, 15, 4, 5, 0, time.UTC))
	c := NewController(f.Context(), fe, fpm, f.Client, st, clock, v1alpha1.NewScheme())

	return &fixture{
		ControllerFixture: f.WithRequeuer(c.requeuer).Build(c),
		st:                st,
		fe:                fe,
		fpm:               fpm,
		sc:                sc,
		c:                 c,
		clock:             clock,
	}
}

func (f *fixture) triggerFileWatch(name string) {
	fw := &FileWatch{}
	err := f.Client.Get(f.Context(), types.NamespacedName{Name: name}, fw)
	require.NoError(f.T(), err)

	fw.Status.LastEventTime = apis.NewMicroTime(f.clock.Now())
	err = f.Client.Status().Update(f.Context(), fw)
	require.NoError(f.T(), err)
}

func (f *fixture) triggerButton(name string, ts time.Time) {
	f.updateButton(name, func(b *v1alpha1.UIButton) {
		b.Status.LastClickedAt = apis.NewMicroTime(ts)
	})
}

func (f *fixture) reconcileCmd(name string) {
	_, err := f.c.Reconcile(f.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}})
	require.NoError(f.T(), err)
}

func (f *fixture) updateSpec(name string, update func(spec *v1alpha1.CmdSpec)) {
	cmd := &Cmd{}
	err := f.Client.Get(f.Context(), types.NamespacedName{Name: name}, cmd)
	require.NoError(f.T(), err)

	update(&(cmd.Spec))
	err = f.Client.Update(f.Context(), cmd)
	require.NoError(f.T(), err)
}

func (f *fixture) updateButton(name string, update func(button *v1alpha1.UIButton)) {
	button := &UIButton{}
	err := f.Client.Get(f.Context(), types.NamespacedName{Name: name}, button)
	require.NoError(f.T(), err)

	update(button)

	copy := button.DeepCopy()
	err = f.Client.Update(f.Context(), button)
	require.NoError(f.T(), err)

	button.Status = copy.Status
	err = f.Client.Status().Update(f.Context(), button)
	require.NoError(f.T(), err)
}

// checks `cmdName`'s DisableSource and makes sure it's configured to be disabled or enabled per `isDisabled`
func (f *fixture) setDisabled(cmdName string, isDisabled bool) {
	cmd := &Cmd{}
	err := f.Client.Get(f.Context(), types.NamespacedName{Name: cmdName}, cmd)
	require.NoError(f.T(), err)

	require.NotNil(f.T(), cmd.Spec.DisableSource)
	require.NotNil(f.T(), cmd.Spec.DisableSource.ConfigMap)

	configMap := &ConfigMap{}
	err = f.Client.Get(f.Context(), types.NamespacedName{Name: cmd.Spec.DisableSource.ConfigMap.Name}, configMap)
	if apierrors.IsNotFound(err) {
		configMap.ObjectMeta.Name = cmd.Spec.DisableSource.ConfigMap.Name
		configMap.Data = map[string]string{cmd.Spec.DisableSource.ConfigMap.Key: strconv.FormatBool(isDisabled)}
		err = f.Client.Create(f.Context(), configMap)
		require.NoError(f.T(), err)
	} else {
		require.Nil(f.T(), err)
		configMap.Data[cmd.Spec.DisableSource.ConfigMap.Key] = strconv.FormatBool(isDisabled)
		err = f.Client.Update(f.Context(), configMap)
		require.NoError(f.T(), err)
	}

	f.reconcileCmd(cmdName)

	var expectedDisableState v1alpha1.DisableState
	if isDisabled {
		expectedDisableState = v1alpha1.DisableStateDisabled
	} else {
		expectedDisableState = v1alpha1.DisableStateEnabled
	}

	// block until the change has been processed
	f.requireCmdMatchesInAPI(cmdName, func(cmd *Cmd) bool {
		return cmd.Status.DisableStatus != nil &&
			cmd.Status.DisableStatus.State == expectedDisableState
	})
}

func (f *fixture) resource(name string, cmd string, workdir string, lastDeploy time.Time) {
	c := model.ToHostCmd(cmd)
	c.Dir = workdir
	localTarget := model.NewLocalTarget(model.TargetName(name), model.Cmd{}, c, nil)
	f.resourceFromTarget(name, localTarget, lastDeploy)
}

func (f *fixture) resourceFromTarget(name string, target model.TargetSpec, lastDeploy time.Time) {
	n := model.ManifestName(name)
	m := model.Manifest{
		Name: n,
	}.WithDeployTarget(target)

	st := f.st.LockMutableStateForTesting()
	defer f.st.UnlockMutableState()

	state := store.NewManifestState(m)
	state.LastSuccessfulDeployTime = lastDeploy
	state.AddCompletedBuild(model.BuildRecord{
		StartTime:  lastDeploy,
		FinishTime: lastDeploy,
	})
	st.UpsertManifestTarget(&store.ManifestTarget{
		Manifest: m,
		State:    state,
	})
}

func (f *fixture) step() {
	f.st.summary = store.ChangeSummary{}
	_ = f.sc.OnChange(f.Context(), f.st, store.LegacyChangeSummary())
	for name := range f.st.summary.CmdSpecs.Changes {
		_, err := f.c.Reconcile(f.Context(), ctrl.Request{NamespacedName: name})
		require.NoError(f.T(), err)
	}
}

func (f *fixture) assertLogMessage(name string, messages ...string) {
	for _, m := range messages {
		assert.Eventually(f.T(), func() bool {
			return strings.Contains(f.Stdout(), m)
		}, timeout, interval)
	}
}

func (f *fixture) waitForLogEventContaining(message string) store.LogAction {
	ctx, cancel := context.WithTimeout(f.Context(), time.Second)
	defer cancel()

	for {
		actions := f.st.Actions()
		for _, action := range actions {
			le, ok := action.(store.LogAction)
			if ok && strings.Contains(string(le.Message()), message) {
				return le
			}
		}
		select {
		case <-ctx.Done():
			f.T().Fatalf("timed out waiting for log event w/ message %q. seen actions: %v", message, actions)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func (f *fixture) assertCmdMatches(name string, matcher func(cmd *Cmd) bool) *Cmd {
	f.T().Helper()
	assert.Eventually(f.T(), func() bool {
		cmd := f.st.Cmd(name)
		if cmd == nil {
			return false
		}
		return matcher(cmd)
	}, timeout, interval)

	return f.requireCmdMatchesInAPI(name, matcher)
}

func (f *fixture) requireCmdMatchesInAPI(name string, matcher func(cmd *Cmd) bool) *Cmd {
	f.T().Helper()
	var cmd Cmd

	require.Eventually(f.T(), func() bool {
		err := f.Client.Get(f.Context(), types.NamespacedName{Name: name}, &cmd)
		require.NoError(f.T(), err)
		return matcher(&cmd)
	}, timeout, interval)

	return &cmd
}

func (f *fixture) assertCmdDeleted(name string) {
	assert.Eventually(f.T(), func() bool {
		cmd := f.st.Cmd(name)
		return cmd == nil || cmd.DeletionTimestamp != nil
	}, timeout, interval)

	var cmd Cmd
	err := f.Client.Get(f.Context(), types.NamespacedName{Name: name}, &cmd)
	assert.Error(f.T(), err)
	assert.True(f.T(), apierrors.IsNotFound(err))
}

func (f *fixture) assertCmdCount(count int) {
	assert.Equal(f.T(), count, f.st.CmdCount())

	var list CmdList
	err := f.Client.List(f.Context(), &list)
	require.NoError(f.T(), err)
	assert.Equal(f.T(), count, len(list.Items))
}
