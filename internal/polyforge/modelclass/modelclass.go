// modelclass.go: the model-class failure classification (polyforge#19).
//
// The text predicates moved here from the driver's supervision table so
// every lane that hands a model candidate to a harness — the driver's
// dispatch, the task lane, the summarize lane, the daemon's submit route —
// shares ONE notion of "model-class unresolvable" (the driver cannot
// import the runtime — its internal tests import runtime — so the shared
// gate sits in a leaf below both). The leaf also owns the dispatch-time
// preflight: the decision, taken BEFORE any process is spawned, that pi
// would refuse a candidate's model at startup, derived from the same
// models.json the spawned step agent actually reads.

package modelclass

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/silgrid/polyforge/internal/common/harness"
)

// AllowanceText is the allowance/auth row of the driver's textClasses
// (model-class, D7): the account or key cannot use this candidate — a
// different one may work, so these switch IMMEDIATELY, even when the
// text also carries a throttle or transport signature ("429 ...
// exceeded your current quota", "insufficient balance ... connection
// reset"). verdictCause promotes a live error turn naming it to
// exit_model_class.
var AllowanceText = regexp.MustCompile(`(?i)(\b401\b|\b403\b|unauthori[sz]ed|authentication|invalid api key|incorrect api key|api key|quota|insufficient|billing|credit|` +
	`余额不足|欠费|(额度|配额)[^，。；,.;\n]{0,6}(用尽|用完|耗尽|不足|超出|超限)|(超出|超过)了?(额度|配额)|` +
	`账户[^，。；,.;\n]{0,10}(停用|禁用|冻结)|鉴权失败|认证失败|无效的?(密钥|令牌)|(密钥|令牌)无效|` +
	`(无权限|没有权限|权限不足)[^，。；,.;\n]{0,6}(模型|接口|API|服务)|(模型|接口|API|服务)[^，。；,.;\n]{0,6}(无权限|没有权限|权限不足))`)

// The name-resolution forms of a live error turn: the pinned model does
// not exist for the provider. verdictCause promotes such a turn to
// exit_model_class (resuming the same unresolvable model cannot help).
// Only ADJACENT forms count — "unknown/unresolved/unsupported/no such
// model", the model_not_found code, or a resolution phrase directly after
// the model's name or id ("model "x" not found", "model x: not found",
// "The model `x` does not exist"). A resolution word anywhere else in a
// sentence that mentions the model is outage-shaped ("The model
// encountered an unknown error", "model server returned unknown status",
// "model endpoint not found, retrying") and stays model_error, as do the
// model-name row's looser shapes ("model ... unavailable", "model ...
// invalid response").
var (
	UnresolvedModelAdjacent = regexp.MustCompile(`(?i)\b(unknown|unresolved|unsupported|no such) model\b|\bmodel_not_found\b`)
	UnresolvedModelNamed    = regexp.MustCompile("(?i)\\bmodel\\W+(\"[^\"]{1,128}\"|'[^']{1,128}'|`[^`]{1,128}`|[^\\s\"'`,;:]{1,128})\\W{0,3}(not found|cannot be resolved|could not be resolved|does not exist)\\b")
)

// ModelInfraNouns are the words that, right after "model", name the
// serving infrastructure rather than a model id: "model endpoint not
// found" is an outage, "model xyz not found" a name that does not resolve.
var ModelInfraNouns = map[string]bool{
	"server": true, "servers": true, "endpoint": true, "endpoints": true, "service": true, "backend": true,
	"provider": true, "gateway": true, "host": true, "instance": true, "instances": true, "worker": true,
	"workers": true, "api": true, "route": true, "upstream": true, "node": true, "pool": true,
	"replica": true, "replicas": true, "container": true, "process": true, "deployment": true,
}

// UnresolvedModel reports whether a live error turn names a model that
// does not resolve (the adjacent forms above).
func UnresolvedModel(text string) bool {
	if UnresolvedModelAdjacent.MatchString(text) {
		return true
	}
	for _, m := range UnresolvedModelNamed.FindAllStringSubmatch(text, -1) {
		if !ModelInfraNouns[strings.ToLower(m[1])] {
			return true
		}
	}
	return false
}

// piBuiltinProviders is the frozen mirror of pi's own built-in provider
// ids (the `builtinProviders()` catalog its installation carries beside
// any models.json). Pi resolves `--model` against the UNION of that
// catalog and the configured file, so a candidate on one of these
// providers is resolvable even when models.json does not name it — the
// preflight never skips them. The list is a mirror, not a probe: a pi
// release adding a provider leaves it unknown HERE until the list is
// refreshed, which can only ever turn into a skip of a candidate the file
// also does not cover (the fallback lane for that: the dispatch refuses
// at startup and the existing model-class death advances the chain).
var piBuiltinProviders = map[string]bool{
	"amazon-bedrock": true, "ant-ling": true, "anthropic": true, "azure": true,
	"baseten": true, "cerebras": true, "cloudflare-ai-gateway": true, "cloudflare-workers-ai": true,
	"deepseek": true, "fireworks": true, "github-copilot": true, "google": true,
	"google-vertex": true, "groq": true, "huggingface": true, "kimi-coding": true,
	"meta": true, "minimax": true, "minimax-cn": true, "minimaxai": true,
	"mistral": true, "moonshotai": true, "moonshotai-cn": true, "nvidia": true,
	"openai": true, "openai-codex": true, "opencode": true, "opencode-go": true,
	"openrouter": true, "qwen-token-plan": true, "qwen-token-plan-cn": true,
	"qwen-token-plan-individual": true, "radius": true, "together": true,
	"typesafe": true, "vercel-ai-gateway": true, "xai": true, "xiaomi": true,
	"xiaomi-token-plan-ams": true, "xiaomi-token-plan-cn": true,
	"xiaomi-token-plan-sgp": true, "zai": true, "zai-coding-cn": true,
}

// modelsFile is the slice of pi's models.json the preflight reads: the
// providers block with each provider's model ids and display names. The
// credential-bearing fields are deliberately not parsed — resolution
// never reads credentials (pi resolves `--model` over ALL models, key
// or no key), and the preflight must not copy secrets into reasons.
type modelsFile struct {
	Providers map[string]modelsProvider `json:"providers"`
}
type modelsProvider struct {
	Models []modelsEntry `json:"models"`
}
type modelsEntry struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// view is one parse of one agent dir's models.json: the model ids and
// names (lowercased for pi's case-insensitive matching) and the provider
// ids that carry at least one model, joined by pi's built-in catalog.
type view struct {
	models    []modelRef
	providers map[string]bool
}
type modelRef struct{ id, name string }

// readView parses agentDir's models.json once. A file that is absent,
// unreadable or unparseable answers ok=false: nothing is proven
// unresolvable, so the caller fails open (every pre-existing test
// fixture, which plants no models.json, keeps today's behavior).
func readView(agentDir string) (*view, bool) {
	if agentDir == "" {
		return nil, false
	}
	raw, err := os.ReadFile(filepath.Join(agentDir, "models.json"))
	if err != nil {
		return nil, false
	}
	var f modelsFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, false
	}
	v := &view{providers: map[string]bool{}}
	for name, p := range f.Providers {
		if len(p.Models) == 0 {
			// a provider with no models has no entry in pi's model pool:
			// it anchors no `provider/model` split
			continue
		}
		v.providers[name] = true
		for _, m := range p.Models {
			v.models = append(v.models, modelRef{
				strings.ToLower(strings.TrimSpace(m.ID)),
				strings.ToLower(strings.TrimSpace(m.Name)),
			})
		}
	}
	for p := range piBuiltinProviders {
		v.providers[p] = true
	}
	return v, true
}

// Predicate answers the per-candidate skip decision over ONE read of
// agentDir's models.json (the driver resolves it once per dispatch,
// outside its mutex): skip=true carries the pi-shaped diagnostic that
// names why the candidate cannot run. It never fails — an unreadable or
// unparseable file resolves to a predicate that skips nothing.
func Predicate(agentDir string) func(harness.Candidate) (bool, string) {
	view, ok := readView(agentDir)
	if !ok {
		return func(harness.Candidate) (bool, string) { return false, "" }
	}
	return view.skip
}

// Preflight is the single-shot form of Predicate (the task lane, the
// summarize lane and the submit route): read agentDir's models.json and
// decide one candidate.
func Preflight(agentDir string, cand harness.Candidate) (skip bool, reason string) {
	view, ok := readView(agentDir)
	if !ok {
		return false, ""
	}
	return view.skip(cand)
}

// skip mirrors pi's `--model` startup resolution far enough to decide
// its one certain refusal: the not-found WITHOUT a provider anchor,
// the diagnostic `Model "<text>" not found. Use --list-models to see
// available models.` Every other shape fails open — no skip:
//
//   - a non-pi harness (claude) has no file-inspectable resolution signal;
//   - a text that matches a configured model id or name (pi matches the
//     id exactly, or by containment over the id and the display name)
//     resolves or is ambiguous across providers — pi decides, loudly,
//     never the preflight;
//   - a `provider/model` whose provider is known (configured with at
//     least one model, or one of pi's built-in catalogs) passes unknown
//     model ids through to the provider as custom model ids — the
//     provider decides at runtime, today's behavior unchanged;
//   - a bare (unqualified) model matches nothing in the FILE but may
//     still match a built-in catalog entry the file cannot see, so a
//     bare miss proves nothing either.
//
// The skip itself is gated by the moved classification — the very
// predicates verdictCause archives exit_model_class with — so no second,
// parallel notion of "unresolvable" exists.
func (v *view) skip(cand harness.Candidate) (bool, string) {
	if cand.Harness != harness.Pi {
		return false, ""
	}
	text := strings.TrimSpace(cand.Model)
	if text == "" || !strings.Contains(text, "/") {
		return false, ""
	}
	lower := strings.ToLower(text)
	for _, m := range v.models {
		if m.id == lower || strings.Contains(m.id, lower) || (m.name != "" && strings.Contains(m.name, lower)) {
			return false, ""
		}
	}
	if v.knownProviderPrefix(text) {
		return false, ""
	}
	diag := fmt.Sprintf("Model %q not found. Use --list-models to see available models.", text)
	if !UnresolvedModel(diag) && !AllowanceText.MatchString(diag) {
		return false, ""
	}
	return true, diag
}

// knownProviderPrefix mirrors pi's provider-prefix split: the text's head
// up to one of its "/" separators — compared case-insensitively after a
// trim, every separator position (provider ids may themselves contain
// "/", pi tries the longest first) — names a provider with at least one
// model, and the model part after it is non-empty.
func (v *view) knownProviderPrefix(text string) bool {
	for i := 0; i < len(text); i++ {
		if text[i] != '/' {
			continue
		}
		head, rest := text[:i], text[i+1:]
		if strings.TrimSpace(head) == "" || strings.TrimSpace(rest) == "" {
			continue
		}
		for p := range v.providers {
			if strings.EqualFold(strings.TrimSpace(p), head) {
				return true
			}
		}
	}
	return false
}
