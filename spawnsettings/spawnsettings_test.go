package spawnsettings

import (
	"os"
	"path/filepath"
	"testing"
)

// writeChannelSettings drops a .claude/settings.json under dir with the given
// raw JSON body and returns dir.
func writeChannelSettings(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, SettingsFile), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestResolve_DefaultsWhenNothingPinned(t *testing.T) {
	cfg := Config{EnvPrefix: "ROSS", Workspace: t.TempDir()}
	model, effort, source, warning := Resolve(cfg)
	if model != DefaultModel {
		t.Errorf("model = %q, want %q", model, DefaultModel)
	}
	if effort != DefaultEffort {
		t.Errorf("effort = %q, want %q", effort, DefaultEffort)
	}
	if source != "model=default,effort=default" {
		t.Errorf("source = %q", source)
	}
	if warning != "" {
		t.Errorf("warning = %q, want empty", warning)
	}
}

func TestResolve_EnvDefaultModel(t *testing.T) {
	t.Setenv("JOANNE_DEFAULT_MODEL", "claude-sonnet-4-6")
	t.Setenv("JOANNE_DEFAULT_EFFORT", "medium")
	model, effort, source, _ := Resolve(Config{EnvPrefix: "JOANNE", Workspace: t.TempDir()})
	if model != "claude-sonnet-4-6" || effort != "medium" {
		t.Fatalf("model=%q effort=%q", model, effort)
	}
	if source != "model=env,effort=env" {
		t.Errorf("source = %q", source)
	}
}

// The regression that took Ross/PA down 2026-06-23: a [1m] env default must be
// rejected (no credits on the pool) and degrade to the working DefaultModel —
// never silently become the source of an outage.
func TestResolve_RejectsOneMByDefault(t *testing.T) {
	t.Setenv("ROSS_DEFAULT_MODEL", "claude-opus-4-8[1m]")
	model, _, source, _ := Resolve(Config{EnvPrefix: "ROSS", Workspace: t.TempDir()})
	if model != DefaultModel {
		t.Errorf("model = %q, want fallback to %q (1m must be rejected)", model, DefaultModel)
	}
	if source != "model=default,effort=default" {
		t.Errorf("source = %q, want default (1m env rejected)", source)
	}
}

func TestResolve_OneMAcceptedWhenOptedIn(t *testing.T) {
	t.Setenv("ROSS_ALLOW_1M_CONTEXT", "true")
	t.Setenv("ROSS_DEFAULT_MODEL", "claude-opus-4-8[1m]")
	model, _, source, _ := Resolve(Config{EnvPrefix: "ROSS", Workspace: t.TempDir()})
	if model != "claude-opus-4-8[1m]" {
		t.Errorf("model = %q, want claude-opus-4-8[1m] when opted in", model)
	}
	if source != "model=env,effort=default" {
		t.Errorf("source = %q", source)
	}
}

func TestResolve_StaleGenerationRejected(t *testing.T) {
	// opus-4-7 is no longer curated; an env pin to it must degrade, not pass.
	t.Setenv("PERSONAL_AGENT_DEFAULT_MODEL", "claude-opus-4-7")
	model, _, _, _ := Resolve(Config{EnvPrefix: "PERSONAL_AGENT", Workspace: t.TempDir()})
	if model != DefaultModel {
		t.Errorf("model = %q, want fallback to %q", model, DefaultModel)
	}
}

func TestResolve_ChannelPinBeatsEnv(t *testing.T) {
	t.Setenv("ROSS_DEFAULT_MODEL", "claude-opus-4-8")
	dir := t.TempDir()
	writeChannelSettings(t, dir, `{"model":"claude-sonnet-4-6","effortLevel":"low"}`)
	model, effort, source, warning := Resolve(Config{EnvPrefix: "ROSS", Workspace: dir})
	if model != "claude-sonnet-4-6" || effort != "low" {
		t.Fatalf("model=%q effort=%q", model, effort)
	}
	if source != "model=channel,effort=channel" {
		t.Errorf("source = %q", source)
	}
	if warning != "" {
		t.Errorf("warning = %q", warning)
	}
}

func TestResolve_InvalidChannelModelWarnsAndFallsBack(t *testing.T) {
	t.Setenv("ROSS_DEFAULT_MODEL", "claude-opus-4-8")
	dir := t.TempDir()
	writeChannelSettings(t, dir, `{"model":"gpt-4o"}`)
	model, _, source, warning := Resolve(Config{EnvPrefix: "ROSS", Workspace: dir})
	if model != "claude-opus-4-8" {
		t.Errorf("model = %q, want env fallback", model)
	}
	if source != "model=env,effort=default" {
		t.Errorf("source = %q", source)
	}
	if warning == "" {
		t.Error("expected a channel-facing warning for the invalid model")
	}
}

func TestResolve_LoopEnvOnlyOnTicks(t *testing.T) {
	t.Setenv("ROSS_DEFAULT_MODEL", "claude-opus-4-8")
	t.Setenv("ROSS_LOOP_MODEL", "claude-sonnet-4-6")
	dir := t.TempDir()

	// Interactive spawn: loop-env ignored.
	model, _, source, _ := Resolve(Config{EnvPrefix: "ROSS", Workspace: dir, IsTick: false})
	if model != "claude-opus-4-8" || source != "model=env,effort=default" {
		t.Errorf("interactive: model=%q source=%q", model, source)
	}

	// Tick spawn: loop-env wins over default-env.
	model, _, source, _ = Resolve(Config{EnvPrefix: "ROSS", Workspace: dir, IsTick: true})
	if model != "claude-sonnet-4-6" || source != "model=loop-env,effort=default" {
		t.Errorf("tick: model=%q source=%q", model, source)
	}
}

func TestResolve_PerLoopOverrideWins(t *testing.T) {
	t.Setenv("ROSS_DEFAULT_MODEL", "claude-opus-4-8")
	dir := t.TempDir()
	writeChannelSettings(t, dir, `{"model":"claude-opus-4-8"}`)
	model, _, source, _ := Resolve(Config{
		EnvPrefix: "ROSS", Workspace: dir, IsTick: true,
		LoopModel: "claude-sonnet-4-6", LoopEffort: "low",
	})
	if model != "claude-sonnet-4-6" {
		t.Errorf("model = %q, want per-loop override", model)
	}
	if source != "model=loop,effort=loop" {
		t.Errorf("source = %q", source)
	}
}

func TestResolve_KeysResolveIndependently(t *testing.T) {
	t.Setenv("ROSS_DEFAULT_EFFORT", "max")
	dir := t.TempDir()
	writeChannelSettings(t, dir, `{"model":"claude-sonnet-4-6"}`)
	model, effort, source, _ := Resolve(Config{EnvPrefix: "ROSS", Workspace: dir})
	if model != "claude-sonnet-4-6" {
		t.Errorf("model = %q", model)
	}
	if effort != "max" {
		t.Errorf("effort = %q, want env max", effort)
	}
	if source != "model=channel,effort=env" {
		t.Errorf("source = %q", source)
	}
}

func TestAllowedModels_Gating(t *testing.T) {
	base := AllowedModels(false)
	if !base["claude-opus-4-8"] || !base["claude-sonnet-4-6"] {
		t.Error("base set missing a curated model")
	}
	if base["claude-opus-4-8[1m]"] {
		t.Error("base set must not include [1m]")
	}
	if base["claude-opus-4-7"] || base["claude-opus-4-7[1m]"] {
		t.Error("stale opus-4-7 generation must not be accepted")
	}
	with1M := AllowedModels(true)
	if !with1M["claude-opus-4-8[1m]"] || !with1M["claude-sonnet-4-6[1m]"] {
		t.Error("opted-in set missing [1m] variants")
	}
}
