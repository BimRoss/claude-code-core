// Package spawnsettings resolves the model + effort the harness passes as
// explicit `claude --model` / `--effort` flags on every spawn. It is the single
// source of truth shared by all three agents (Ross, Joanne, personal-agent),
// replacing the per-wrapper copies that drifted apart (divergent allowlists,
// stale [1m] defaults) and let a bad pin fall back to a broken model.
//
// Policy in one place:
//   - AllowedModels is deliberately small — one Sonnet (cheap/background) and
//     one Opus (interactive). Adding a model is a one-line change here, not in
//     three repos.
//   - The 1M-context ([1m]) variants are OFF by default. The shared OAuth pool
//     has no usage-based billing, so 1M requests fail with "Usage credits
//     required for 1M context" — and opus-4-7[1m] *hung* rather than erroring
//     cleanly, which took Ross + the personal agents offline on 2026-06-23.
//     Re-enable per agent via <PREFIX>_ALLOW_1M_CONTEXT once credits are on.
//   - DefaultModel is a working, non-1M model, so an invalid/stale pin degrades
//     to something that runs instead of something that hangs.
package spawnsettings

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
)

// baseModels are the curated 200k-context models the harness will pass to
// `claude --model`. Keep this list small and current: bumping the Opus or
// Sonnet generation is a one-line edit here that reaches every agent.
var baseModels = []string{
	"claude-sonnet-4-6",
	"claude-opus-4-8",
}

// oneMModels are the 1M-context counterparts, admitted only when an agent sets
// <PREFIX>_ALLOW_1M_CONTEXT (see AllowedModels). Kept in lockstep with
// baseModels so re-enabling 1M never resurrects a stale generation.
var oneMModels = []string{
	"claude-sonnet-4-6[1m]",
	"claude-opus-4-8[1m]",
}

// allowedEfforts is the set of effort levels the harness will pass to
// `claude --effort`. `xhigh` and `max` are both accepted by the CLI even though
// older docs only listed up to `xhigh`.
var allowedEfforts = map[string]bool{
	"low":    true,
	"medium": true,
	"high":   true,
	"xhigh":  true,
	"max":    true,
}

const (
	// DefaultModel / DefaultEffort are the floor of the precedence ladder — what
	// a spawn gets when nothing else pins a value, and where an invalid pin
	// degrades to. Must always be a non-1M model that runs on the pool.
	DefaultModel  = "claude-opus-4-8"
	DefaultEffort = "high"

	// SettingsFile is the per-workspace overrides file an agent-in-a-channel can
	// edit to change the model/effort used for the *next* spawn. Sits at
	// <workspace>/.claude/settings.json to line up with Claude Code's own
	// project-scope config path, though the harness reads it directly.
	SettingsFile = ".claude/settings.json"
)

// AllowedModels returns the accepted `--model` set. When allow1M is false (the
// default) only the curated 200k models are accepted; the [1m] variants are
// rejected so the pool's missing 1M credits can't take an agent down. Flip it
// per agent via <PREFIX>_ALLOW_1M_CONTEXT once usage credits are enabled.
func AllowedModels(allow1M bool) map[string]bool {
	m := make(map[string]bool, len(baseModels)+len(oneMModels))
	for _, name := range baseModels {
		m[name] = true
	}
	if allow1M {
		for _, name := range oneMModels {
			m[name] = true
		}
	}
	return m
}

// AllowedEfforts returns the accepted `--effort` set.
func AllowedEfforts() map[string]bool {
	out := make(map[string]bool, len(allowedEfforts))
	for k, v := range allowedEfforts {
		out[k] = v
	}
	return out
}

// channelSettings is the on-disk shape parsed from SettingsFile. Only `model`
// and `effortLevel` are harness-controlled; anything else (e.g.
// enableAllProjectMcpServers) is ignored here and left for the CLI.
type channelSettings struct {
	Model       *string `json:"model,omitempty"`
	EffortLevel *string `json:"effortLevel,omitempty"`
}

// Config parameterizes Resolve for a specific agent.
type Config struct {
	// EnvPrefix is the agent's env-var namespace, e.g. "ROSS", "JOANNE",
	// "PERSONAL_AGENT". Resolve reads <PREFIX>_DEFAULT_MODEL, _DEFAULT_EFFORT,
	// _LOOP_MODEL, _LOOP_EFFORT, and _ALLOW_1M_CONTEXT.
	EnvPrefix string
	// Workspace is the channel's PVC dir; Resolve reads <Workspace>/SettingsFile.
	Workspace string
	// LoopModel / LoopEffort are per-loop overrides from a loop entry's
	// Model / EffortLevel fields — non-empty only on loop-tick spawns.
	LoopModel  string
	LoopEffort string
	// IsTick marks loop/watch-driven (unattended) spawns, enabling the
	// background-tier <PREFIX>_LOOP_* env defaults.
	IsTick bool
}

func (c Config) env(suffix string) string { return os.Getenv(c.EnvPrefix + "_" + suffix) }

// allow1M reports whether this agent opts into the 1M-context variants.
func (c Config) allow1M() bool {
	v := c.env("ALLOW_1M_CONTEXT")
	if v == "" {
		return false
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		slog.Warn("allow_1m_context_invalid", "value", v)
		return false
	}
	return b
}

// Resolve picks the model + effort for the next claude spawn in this workspace.
// Each key resolves independently up the precedence ladder so a channel that
// only pins `model` still gets env/default effort.
//
// Precedence (high → low), each key independent:
//
//	model:                                  effort:
//	  1. LoopModel  (tick spawns only)        1. LoopEffort (tick spawns only)
//	  2. <Workspace>/.claude/settings.json    2. <Workspace>/.claude/settings.json
//	  3. <PREFIX>_LOOP_MODEL  (ticks only)    3. <PREFIX>_LOOP_EFFORT (ticks only)
//	  4. <PREFIX>_DEFAULT_MODEL               4. <PREFIX>_DEFAULT_EFFORT
//	  5. DefaultModel constant                5. DefaultEffort constant
//
// Returns the resolved model, effort, a compact source string for logging
// (e.g. "model=loop-env,effort=loop"), and a human-readable warning that is
// non-empty only when the channel file held an invalid value — the caller
// should post it back to the channel so the typo is visible. Invalid loop/env
// values are slog-warned but not surfaced to Slack, since they're
// harness/operator config the channel author can't typo-fix mid-tick.
func Resolve(cfg Config) (model, effort, source, warning string) {
	allowedModels := AllowedModels(cfg.allow1M())

	var (
		chModel, chEffort       string
		chHasModel, chHasEffort bool
		warnings                []string
	)

	path := filepath.Join(cfg.Workspace, SettingsFile)
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		var s channelSettings
		if jerr := json.Unmarshal(data, &s); jerr != nil {
			slog.Warn("spawn_settings_parse_failed", "path", path, "error", jerr)
			warnings = append(warnings, fmt.Sprintf("couldn't parse %s (%v) — falling back to defaults", SettingsFile, jerr))
		} else {
			if s.Model != nil {
				v := *s.Model
				if allowedModels[v] {
					chModel, chHasModel = v, true
				} else {
					slog.Warn("spawn_settings_invalid_model", "path", path, "value", v)
					warnings = append(warnings, fmt.Sprintf("ignoring invalid model %q in %s — falling back", v, SettingsFile))
				}
			}
			if s.EffortLevel != nil {
				v := *s.EffortLevel
				if allowedEfforts[v] {
					chEffort, chHasEffort = v, true
				} else {
					slog.Warn("spawn_settings_invalid_effort", "path", path, "value", v)
					warnings = append(warnings, fmt.Sprintf("ignoring invalid effortLevel %q in %s — falling back", v, SettingsFile))
				}
			}
		}
	case os.IsNotExist(err):
		// expected: most channels never pin overrides
	default:
		slog.Warn("spawn_settings_read_failed", "path", path, "error", err)
	}

	envModel := cfg.env("DEFAULT_MODEL")
	envEffort := cfg.env("DEFAULT_EFFORT")

	// Env values are pod-level operator config — if someone set an unknown
	// value, log it once and fall through to the constant. Don't surface to
	// Slack since the channel author can't fix env vars.
	if envModel != "" && !allowedModels[envModel] {
		slog.Warn("env_default_model_invalid", "value", envModel)
		envModel = ""
	}
	if envEffort != "" && !allowedEfforts[envEffort] {
		slog.Warn("env_default_effort_invalid", "value", envEffort)
		envEffort = ""
	}

	// Background-tier defaults: consulted only on loop/watch ticks (IsTick), so
	// unattended recurring spawns can drop to a cheaper model/effort without
	// touching the interactive default a human sees. Sit below a channel pin but
	// above the global <PREFIX>_DEFAULT_* tier. Validated here; invalid → fall
	// through.
	var loopEnvModel, loopEnvEffort string
	if cfg.IsTick {
		loopEnvModel = cfg.env("LOOP_MODEL")
		loopEnvEffort = cfg.env("LOOP_EFFORT")
		if loopEnvModel != "" && !allowedModels[loopEnvModel] {
			slog.Warn("loop_env_model_invalid", "value", loopEnvModel)
			loopEnvModel = ""
		}
		if loopEnvEffort != "" && !allowedEfforts[loopEnvEffort] {
			slog.Warn("loop_env_effort_invalid", "value", loopEnvEffort)
			loopEnvEffort = ""
		}
	}

	// Per-loop overrides are highest-precedence for their key but validated here
	// (not at registry write time) so a hand-edited loops.json can't sneak a
	// bogus value past the allowlist. Invalid → fall through silently.
	var loopHasModel bool
	if cfg.LoopModel != "" {
		if allowedModels[cfg.LoopModel] {
			loopHasModel = true
		} else {
			slog.Warn("loop_model_invalid", "value", cfg.LoopModel)
		}
	}
	var loopHasEffort bool
	if cfg.LoopEffort != "" {
		if allowedEfforts[cfg.LoopEffort] {
			loopHasEffort = true
		} else {
			slog.Warn("loop_effort_invalid", "value", cfg.LoopEffort)
		}
	}

	var modelSrc, effortSrc string
	switch {
	case loopHasModel:
		model, modelSrc = cfg.LoopModel, "loop"
	case chHasModel:
		model, modelSrc = chModel, "channel"
	case loopEnvModel != "":
		model, modelSrc = loopEnvModel, "loop-env"
	case envModel != "":
		model, modelSrc = envModel, "env"
	default:
		model, modelSrc = DefaultModel, "default"
	}
	switch {
	case loopHasEffort:
		effort, effortSrc = cfg.LoopEffort, "loop"
	case chHasEffort:
		effort, effortSrc = chEffort, "channel"
	case loopEnvEffort != "":
		effort, effortSrc = loopEnvEffort, "loop-env"
	case envEffort != "":
		effort, effortSrc = envEffort, "env"
	default:
		effort, effortSrc = DefaultEffort, "default"
	}

	source = fmt.Sprintf("model=%s,effort=%s", modelSrc, effortSrc)
	if len(warnings) > 0 {
		warning = joinWarnings(warnings)
	}
	return
}

func joinWarnings(ws []string) string {
	out := ws[0]
	for _, w := range ws[1:] {
		out += "; " + w
	}
	return out
}
