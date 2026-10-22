package config

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// SOTAModels is the fixed default order of the built-in sota group. A custom
// group may reuse any model, including these, by listing it explicitly.
var SOTAModels = []string{"deepseek-v4.1-flash", "glm-5.3", "grok-4.6", "kimi-k3", "qwen3.8-max"}

// RotationGroup describes one rotation slot. A group is either static (an
// explicit Order) or dynamic (a Query re-evaluated on every model refresh);
// the two are mutually exclusive.
type RotationGroup struct {
	Alias string   `json:"alias"`
	Order []string `json:"order,omitempty"`
	// Query turns the group dynamic: membership is recomputed from model facts
	// on every catalog and pricing refresh.
	Query *RotationQuery `json:"query,omitempty"`
	// Sort orders matched models. When nil (or direction "none") the previous
	// priority is preserved and new matches are appended last.
	Sort *GroupSort `json:"sort,omitempty"`
	// Pin forces matched models to the front in the given order. Pins that no
	// longer match stay configured but are inactive until they match again.
	Pin []string `json:"pin,omitempty"`
}

// RotationQuery is a structured, UI-built model filter. It is a sequence of
// terms combined left to right: each term after the first carries the "and" or
// "or" operator that joins it to the running result. A term is either a single
// Condition or a nested RotationQuery group, so conditions can be grouped and
// nested arbitrarily.
type RotationQuery struct {
	Terms []QueryTerm `json:"terms"`
}

// QueryTerm is one element of a RotationQuery sequence. Exactly one of
// Condition and Group is set. Op is the operator joining this term to the
// previous one ("and" default, "or"); it is ignored on the first term.
type QueryTerm struct {
	Op        string         `json:"op,omitempty"`
	Condition *Condition     `json:"condition,omitempty"`
	Group     *RotationQuery `json:"group,omitempty"`
}

// maxQueryDepth bounds query nesting so a pathological config cannot build an
// unbounded tree.
const maxQueryDepth = 4

// Condition is one field/operator/value row of a RotationQuery.
type Condition struct {
	Field string `json:"field"`
	Op    string `json:"op"`
	Value any    `json:"value"`
}

// GroupSort orders a dynamic group's matched models by a numeric or string
// field.
type GroupSort struct {
	Field     string `json:"field"`
	Direction string `json:"direction,omitempty"` // "asc" (default) | "desc"
}

// RotationConfig configures the built-in rotation groups and any custom
// groups, static or query-driven. FirstOutputSeconds bounds how long an attempt
// may take to produce its first output before the next attempt runs.
type RotationConfig struct {
	SOTA RotationGroup `json:"sota"`
	// Sweet is the built-in fallback group. An empty Order lets it absorb every
	// available non-SOTA model; a non-empty Order narrows it (models are kept as
	// they appear, availability is still resolved at runtime).
	Sweet RotationGroup `json:"sweet"`
	// Custom holds operator-defined rotation groups. Each one rotates only the
	// models named in its Order (static) or matched by its Query (dynamic) and
	// never auto-populates like Sweet.
	Custom             []RotationGroup `json:"custom,omitempty"`
	FirstOutputSeconds int             `json:"first_output_seconds"`
}

// Dynamic reports whether the group recomputes membership from a query.
func (g RotationGroup) Dynamic() bool {
	return g.Query != nil && g.Query.hasTerms()
}

// hasTerms reports whether the query carries at least one condition or nested
// group anywhere in the tree.
func (q RotationQuery) hasTerms() bool {
	for _, term := range q.Terms {
		if term.Condition != nil || (term.Group != nil && term.Group.hasTerms()) {
			return true
		}
	}
	return false
}

// normalize fills in the default "and" operator on non-first terms across the
// whole query tree.
func (q *RotationQuery) normalize() {
	if q == nil {
		return
	}
	for i := range q.Terms {
		if i == 0 {
			q.Terms[i].Op = ""
		} else if strings.TrimSpace(q.Terms[i].Op) == "" {
			q.Terms[i].Op = "and"
		}
		q.Terms[i].Group.normalize()
	}
}

// IsSOTA reports whether model belongs to the built-in SOTA pool.
func IsSOTA(model string) bool { return slices.Contains(SOTAModels, model) }

// All returns every group in display order: the two built-ins first, then the
// custom groups.
func (c RotationConfig) All() []RotationGroup {
	groups := make([]RotationGroup, 0, 2+len(c.Custom))
	groups = append(groups, c.SOTA, c.Sweet)
	groups = append(groups, c.Custom...)
	return groups
}

func (c *RotationConfig) Defaults() {
	if c.SOTA.Alias == "" {
		c.SOTA.Alias = "sota"
	}
	if c.Sweet.Alias == "" {
		c.Sweet.Alias = "sweet"
	}
	if len(c.SOTA.Order) == 0 {
		c.SOTA.Order = slices.Clone(SOTAModels)
	}
	if c.FirstOutputSeconds == 0 {
		c.FirstOutputSeconds = 30
	}
	for i := range c.Custom {
		if c.Custom[i].Alias == "" {
			c.Custom[i].Alias = fmt.Sprintf("custom-%d", i+1)
		}
		c.Custom[i].Query.normalize()
		if s := c.Custom[i].Sort; s != nil && strings.TrimSpace(s.Direction) == "" {
			s.Direction = "asc"
		}
	}
}

// Validate requires a first-output timeout of 1-300 seconds; distinct aliases
// of up to 64 letters, digits, dots, dashes or underscores, starting with a
// letter or digit, that do not shadow a model ID (one of realModels, SOTAModels
// or any group order); and orders without empty or repeated entries, where the
// SOTA group holds exactly SOTAModels, the sweet group only other models, and
// custom groups are non-empty. A nil realModels skips the catalog check.
func (c RotationConfig) Validate(realModels []string) error {
	if c.FirstOutputSeconds < 1 || c.FirstOutputSeconds > 300 {
		return fmt.Errorf("首个有效输出超时需为 1–300 秒")
	}
	groups := c.All()
	// Models named anywhere. An alias may not shadow a model that another group
	// rotates, because routing resolves aliases before real model IDs.
	models := map[string]bool{}
	for i, g := range groups {
		if g.Query != nil && !g.Query.hasTerms() {
			return fmt.Errorf("查询分组至少需要一个条件")
		}
		dynamic := g.Dynamic()
		if dynamic && len(g.Order) > 0 {
			return fmt.Errorf("分组不能同时包含模型列表和查询条件")
		}
		seen := map[string]bool{}
		for _, model := range g.Order {
			if model == "" || seen[model] {
				return fmt.Errorf("模型顺序包含重复或空值")
			}
			seen[model] = true
			models[model] = true
		}
		pins := map[string]bool{}
		for _, pin := range g.Pin {
			if pin == "" || pins[pin] {
				return fmt.Errorf("固定优先模型包含重复或空值")
			}
			pins[pin] = true
		}
		switch i {
		case 0:
			if dynamic || g.Sort != nil || len(g.Pin) > 0 {
				return fmt.Errorf("SOTA 组不支持查询、排序或固定优先")
			}
			if len(g.Order) != len(SOTAModels) {
				return fmt.Errorf("SOTA 组必须包含指定的五个模型")
			}
			for _, model := range g.Order {
				if !IsSOTA(model) {
					return fmt.Errorf("SOTA 组只能包含指定的模型")
				}
			}
		case 1:
			if dynamic || g.Sort != nil || len(g.Pin) > 0 {
				return fmt.Errorf("甜点组不支持查询、排序或固定优先")
			}
			for _, model := range g.Order {
				if IsSOTA(model) {
					return fmt.Errorf("甜点组不能包含 SOTA 模型")
				}
			}
		default:
			if !dynamic {
				if len(g.Order) == 0 {
					return fmt.Errorf("自定义分组至少需要一个模型或查询条件")
				}
				if g.Sort != nil || len(g.Pin) > 0 {
					return fmt.Errorf("排序与固定优先仅适用于查询分组")
				}
				break
			}
			if err := g.Query.Validate(); err != nil {
				return err
			}
			if g.Sort != nil {
				if err := g.Sort.Validate(); err != nil {
					return err
				}
			}
		}
	}
	valid := regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	aliases := map[string]bool{}
	for _, g := range groups {
		if !valid.MatchString(g.Alias) {
			return fmt.Errorf("别名需为 1–64 位英文字母、数字、点、短横线或下划线")
		}
		if aliases[g.Alias] || IsSOTA(g.Alias) || models[g.Alias] || slices.Contains(realModels, g.Alias) {
			return fmt.Errorf("别名不能与真实模型 ID 或其它分组相同")
		}
		aliases[g.Alias] = true
	}
	return nil
}

// CloneRotationGroup deep-copies a group, including its query condition values.
func CloneRotationGroup(group RotationGroup) RotationGroup {
	group.Order = append([]string(nil), group.Order...)
	group.Pin = append([]string(nil), group.Pin...)
	if group.Sort != nil {
		sortCopy := *group.Sort
		group.Sort = &sortCopy
	}
	if group.Query != nil {
		queryCopy := cloneQuery(*group.Query)
		group.Query = &queryCopy
	}
	return group
}

// cloneQuery deep-copies a query tree, including condition values.
func cloneQuery(q RotationQuery) RotationQuery {
	out := RotationQuery{Terms: make([]QueryTerm, len(q.Terms))}
	for i, term := range q.Terms {
		out.Terms[i] = term
		if term.Condition != nil {
			condition := *term.Condition
			condition.Value = cloneQueryValue(condition.Value)
			out.Terms[i].Condition = &condition
		}
		if term.Group != nil {
			group := cloneQuery(*term.Group)
			out.Terms[i].Group = &group
		}
	}
	return out
}

func cloneQueryValue(value any) any {
	switch typed := value.(type) {
	case []string:
		return append([]string(nil), typed...)
	case []any:
		return append([]any(nil), typed...)
	default:
		return value
	}
}

// QueryField is the UI-facing description of one queryable model field.
type QueryField struct {
	Name string   `json:"name"`
	Kind string   `json:"kind"`
	Ops  []string `json:"ops"`
	// Values lists the accepted values for a closed field (protocol, tier,
	// modalities); empty means the value is free-form. The WebUI renders a
	// dropdown or checkbox group when it is set.
	Values []string `json:"values,omitempty"`
	// Hint is a short guidance string shown next to the value editor.
	Hint string `json:"hint,omitempty"`
}

// ProtocolValues are the wire protocols a model can be served with. SystemOne
// is never rotated but is still a valid protocol value.
var ProtocolValues = []string{"chat", "responses", "anthropic", "systemone"}

// TierValues are the key tiers a model can be served from.
var TierValues = []string{string(TierZen), string(TierGo)}

// ModalityValues are the input/output modalities reported by the catalog.
var ModalityValues = []string{"text", "image", "audio", "video", "pdf"}

// QueryFields lists every field the WebUI may build conditions from.
func QueryFields() []QueryField {
	order := []string{
		"free", "deprecated", "sota", "routable", "cost_known", "reasoning", "tool_call", "structured_output",
		"context_window", "max_input", "max_output", "cost.input", "cost.output", "cost.cache_read", "cost.cache_write",
		"id", "protocol", "tier", "input_modalities", "output_modalities",
	}
	fields := make([]QueryField, 0, len(order))
	for _, name := range order {
		spec := rotationFieldSpecs[name]
		field := QueryField{Name: name, Kind: spec.kind.String(), Ops: append([]string(nil), spec.ops...)}
		switch name {
		case "protocol":
			field.Values = append([]string(nil), ProtocolValues...)
			field.Hint = "chat / responses / anthropic"
		case "tier":
			field.Values = append([]string(nil), TierValues...)
			field.Hint = "zen / go"
		case "input_modalities", "output_modalities":
			field.Values = append([]string(nil), ModalityValues...)
			field.Hint = "多选，如 text / image"
		case "id":
			field.Hint = "模型 ID，如 glm-5.3"
		case "context_window", "max_input", "max_output":
			field.Hint = "以 token 计，如 200000"
		case "cost.input", "cost.output", "cost.cache_read", "cost.cache_write":
			field.Hint = "美元 / 百万 token，如 1"
		}
		fields = append(fields, field)
	}
	return fields
}

// SortFields lists the fields a dynamic group may be sorted by.
func SortFields() []QueryField {
	order := []string{"context_window", "max_input", "max_output", "cost.input", "cost.output", "cost.cache_read", "cost.cache_write", "id", "protocol"}
	fields := make([]QueryField, 0, len(order))
	for _, name := range order {
		spec := rotationFieldSpecs[name]
		fields = append(fields, QueryField{Name: name, Kind: spec.kind.String()})
	}
	return fields
}

// SortDirections lists the accepted sort directions; "none" keeps stable order.
func SortDirections() []string { return []string{"none", "asc", "desc"} }

func (q RotationQuery) Validate() error {
	return q.validate(0)
}

func (q RotationQuery) validate(depth int) error {
	if depth > maxQueryDepth {
		return fmt.Errorf("查询分组嵌套过深")
	}
	if !q.hasTerms() {
		return fmt.Errorf("查询条件不能为空")
	}
	for i, term := range q.Terms {
		if i > 0 {
			switch op := strings.ToLower(strings.TrimSpace(term.Op)); op {
			case "", "and", "or":
			default:
				return fmt.Errorf("查询运算符需为 and 或 or")
			}
		}
		hasCondition := term.Condition != nil
		hasGroup := term.Group != nil
		if hasCondition == hasGroup {
			return fmt.Errorf("查询条件项需且仅需包含一个条件或分组")
		}
		if hasCondition {
			condition := *term.Condition
			spec, ok := rotationFieldSpecs[condition.Field]
			if !ok {
				return fmt.Errorf("未知的查询字段：%s", condition.Field)
			}
			if !slices.Contains(spec.ops, condition.Op) {
				return fmt.Errorf("字段 %s 不支持操作符 %s", condition.Field, condition.Op)
			}
			if err := validateConditionValue(spec, condition); err != nil {
				return err
			}
			continue
		}
		if !term.Group.hasTerms() {
			return fmt.Errorf("查询分组不能为空")
		}
		if err := term.Group.validate(depth + 1); err != nil {
			return err
		}
	}
	return nil
}

func validateConditionValue(spec fieldSpec, condition Condition) error {
	switch spec.kind {
	case kindBool:
		if _, ok := coerceBool(condition.Value); !ok {
			return fmt.Errorf("字段 %s 需要布尔值", condition.Field)
		}
	case kindNumber:
		if _, ok := coerceFloat(condition.Value); !ok {
			return fmt.Errorf("字段 %s 需要数值", condition.Field)
		}
	case kindString:
		if condition.Op == "in" {
			if _, ok := coerceStrings(condition.Value); !ok {
				return fmt.Errorf("字段 %s 的 in 操作符需要字符串数组", condition.Field)
			}
		} else if _, ok := coerceString(condition.Value); !ok {
			return fmt.Errorf("字段 %s 需要字符串", condition.Field)
		}
	case kindList:
		if _, ok := coerceStrings(condition.Value); !ok {
			return fmt.Errorf("字段 %s 需要字符串数组", condition.Field)
		}
	}
	return nil
}

func (s GroupSort) Validate() error {
	spec, ok := rotationFieldSpecs[s.Field]
	if !ok || !spec.sortable {
		return fmt.Errorf("未知或不可排序的字段：%s", s.Field)
	}
	switch strings.ToLower(strings.TrimSpace(s.Direction)) {
	case "", "asc", "desc", "none":
		return nil
	default:
		return fmt.Errorf("排序方向需为 asc、desc 或 none")
	}
}

// ModelFacts is the queryable projection of one catalog model. It is produced
// by the models package and consumed by query evaluation and ordering.
type ModelFacts struct {
	ID               string   `json:"id"`
	Tier             []string `json:"tier"`
	Protocol         string   `json:"protocol"`
	Free             bool     `json:"free"`
	Deprecated       bool     `json:"deprecated"`
	SOTA             bool     `json:"sota"`
	Routable         bool     `json:"routable"`
	Reasoning        bool     `json:"reasoning"`
	ToolCall         bool     `json:"tool_call"`
	StructuredOutput bool     `json:"structured_output"`
	ContextWindow    int      `json:"context_window"`
	MaxInput         int      `json:"max_input"`
	MaxOutput        int      `json:"max_output"`
	InputModalities  []string `json:"input_modalities"`
	OutputModalities []string `json:"output_modalities"`
	CostInput        *float64 `json:"cost_input,omitempty"`
	CostOutput       *float64 `json:"cost_output,omitempty"`
	CostRead         *float64 `json:"cost_read,omitempty"`
	CostWrite        *float64 `json:"cost_write,omitempty"`
}

// Evaluate reports whether the model satisfies the query. Terms are combined
// left to right: the first term seeds the result, then each following term is
// joined with its "and"/"or" operator. An empty query never matches.
func (q RotationQuery) Evaluate(f ModelFacts) bool {
	if len(q.Terms) == 0 {
		return false
	}
	result := q.Terms[0].evaluate(f)
	for _, term := range q.Terms[1:] {
		value := term.evaluate(f)
		if strings.EqualFold(strings.TrimSpace(term.Op), "or") {
			result = result || value
		} else {
			result = result && value
		}
	}
	return result
}

func (t QueryTerm) evaluate(f ModelFacts) bool {
	switch {
	case t.Condition != nil:
		return t.Condition.match(f)
	case t.Group != nil:
		return t.Group.Evaluate(f)
	default:
		return false
	}
}

// ResolveOrder computes a dynamic group's priority: pins first, then the
// remaining matches sorted by the optional sort, or else kept in their previous
// relative order with new matches appended (lowest priority) alphabetically.
func ResolveOrder(matched []string, facts map[string]ModelFacts, groupSort *GroupSort, pins, previous []string) []string {
	matchSet := make(map[string]bool, len(matched))
	for _, id := range matched {
		matchSet[id] = true
	}
	used := make(map[string]bool, len(matched))
	order := make([]string, 0, len(matched))
	for _, pin := range pins {
		if matchSet[pin] && !used[pin] {
			order = append(order, pin)
			used[pin] = true
		}
	}
	rest := make([]string, 0, len(matched))
	for _, id := range matched {
		if !used[id] {
			rest = append(rest, id)
		}
	}
	if groupSort != nil && groupSort.Field != "" && !strings.EqualFold(strings.TrimSpace(groupSort.Direction), "none") {
		sortFacts(rest, facts, *groupSort)
	} else {
		rest = stableOrder(rest, previous)
	}
	return append(order, rest...)
}

func stableOrder(rest, previous []string) []string {
	inRest := make(map[string]bool, len(rest))
	for _, id := range rest {
		inRest[id] = true
	}
	seen := make(map[string]bool, len(rest))
	out := make([]string, 0, len(rest))
	for _, id := range previous {
		if inRest[id] && !seen[id] {
			out = append(out, id)
			seen[id] = true
		}
	}
	extra := make([]string, 0)
	for _, id := range rest {
		if !seen[id] {
			extra = append(extra, id)
		}
	}
	sort.Strings(extra)
	return append(out, extra...)
}

func sortFacts(ids []string, facts map[string]ModelFacts, groupSort GroupSort) {
	desc := strings.EqualFold(strings.TrimSpace(groupSort.Direction), "desc")
	kind := rotationFieldSpecs[groupSort.Field].kind
	sort.SliceStable(ids, func(i, j int) bool {
		ai, si, oki := sortKey(facts[ids[i]], groupSort.Field)
		aj, sj, okj := sortKey(facts[ids[j]], groupSort.Field)
		if oki != okj {
			return oki
		}
		if !oki {
			return ids[i] < ids[j]
		}
		if kind == kindString {
			if si != sj {
				if desc {
					return si > sj
				}
				return si < sj
			}
			return ids[i] < ids[j]
		}
		if ai != aj {
			if desc {
				return ai > aj
			}
			return ai < aj
		}
		return ids[i] < ids[j]
	})
}

func sortKey(f ModelFacts, field string) (float64, string, bool) {
	switch field {
	case "id":
		return 0, f.ID, true
	case "protocol":
		return 0, f.Protocol, true
	}
	if value, ok := factNumber(f, field); ok {
		return value, "", true
	}
	return 0, "", false
}

func (c Condition) match(f ModelFacts) bool {
	spec, ok := rotationFieldSpecs[c.Field]
	if !ok {
		return false
	}
	switch spec.kind {
	case kindBool:
		want, ok := coerceBool(c.Value)
		if !ok {
			return false
		}
		got, ok := factBool(f, c.Field)
		return ok && got == want
	case kindNumber:
		got, known := factNumber(f, c.Field)
		if !known {
			return false
		}
		want, ok := coerceFloat(c.Value)
		if !ok {
			return false
		}
		return compareNumber(got, c.Op, want)
	case kindString:
		got := factString(f, c.Field)
		if c.Op == "in" {
			want, ok := coerceStrings(c.Value)
			if !ok {
				return false
			}
			return slices.Contains(want, got)
		}
		want, ok := coerceString(c.Value)
		if !ok {
			return false
		}
		return compareString(got, c.Op, want)
	case kindList:
		want, ok := coerceStrings(c.Value)
		if !ok {
			return false
		}
		return compareList(factList(f, c.Field), c.Op, want)
	}
	return false
}

func compareNumber(got float64, op string, want float64) bool {
	switch op {
	case ">=":
		return got >= want
	case ">":
		return got > want
	case "<=":
		return got <= want
	case "<":
		return got < want
	case "==":
		return got == want
	case "!=":
		return got != want
	}
	return false
}

func compareString(got, op, want string) bool {
	switch op {
	case "equals":
		return got == want
	case "contains":
		return strings.Contains(got, want)
	case "starts_with":
		return strings.HasPrefix(got, want)
	case "ends_with":
		return strings.HasSuffix(got, want)
	}
	return false
}

func compareList(got []string, op string, want []string) bool {
	switch op {
	case "has_any":
		for _, value := range want {
			if slices.Contains(got, value) {
				return true
			}
		}
		return false
	case "has_all":
		for _, value := range want {
			if !slices.Contains(got, value) {
				return false
			}
		}
		return len(want) > 0
	case "not_has":
		for _, value := range want {
			if slices.Contains(got, value) {
				return false
			}
		}
		return true
	}
	return false
}

func factBool(f ModelFacts, field string) (bool, bool) {
	switch field {
	case "free":
		return f.Free, true
	case "deprecated":
		return f.Deprecated, true
	case "sota":
		return f.SOTA, true
	case "routable":
		return f.Routable, true
	case "cost_known":
		return f.CostInput != nil || f.CostOutput != nil, true
	case "reasoning":
		return f.Reasoning, true
	case "tool_call":
		return f.ToolCall, true
	case "structured_output":
		return f.StructuredOutput, true
	}
	return false, false
}

func factNumber(f ModelFacts, field string) (float64, bool) {
	switch field {
	case "context_window":
		return float64(f.ContextWindow), true
	case "max_input":
		return float64(f.MaxInput), true
	case "max_output":
		return float64(f.MaxOutput), true
	case "cost.input":
		return pointerValue(f.CostInput)
	case "cost.output":
		return pointerValue(f.CostOutput)
	case "cost.cache_read":
		return pointerValue(f.CostRead)
	case "cost.cache_write":
		return pointerValue(f.CostWrite)
	}
	return 0, false
}

func factString(f ModelFacts, field string) string {
	switch field {
	case "id":
		return f.ID
	case "protocol":
		return f.Protocol
	}
	return ""
}

func factList(f ModelFacts, field string) []string {
	switch field {
	case "tier":
		return f.Tier
	case "input_modalities":
		return f.InputModalities
	case "output_modalities":
		return f.OutputModalities
	}
	return nil
}

func pointerValue(value *float64) (float64, bool) {
	if value == nil {
		return 0, false
	}
	return *value, true
}

func coerceBool(value any) (bool, bool) {
	boolean, ok := value.(bool)
	return boolean, ok
}

func coerceFloat(value any) (float64, bool) {
	switch number := value.(type) {
	case float64:
		return number, true
	case float32:
		return float64(number), true
	case int:
		return float64(number), true
	case int64:
		return float64(number), true
	case json.Number:
		parsed, err := number.Float64()
		return parsed, err == nil
	}
	return 0, false
}

func coerceString(value any) (string, bool) {
	text, ok := value.(string)
	return text, ok
}

func coerceStrings(value any) ([]string, bool) {
	switch list := value.(type) {
	case []string:
		return list, true
	case []any:
		out := make([]string, 0, len(list))
		for _, item := range list {
			text, ok := item.(string)
			if !ok {
				return nil, false
			}
			out = append(out, text)
		}
		return out, true
	}
	return nil, false
}

type fieldKind int

const (
	kindBool fieldKind = iota
	kindNumber
	kindString
	kindList
)

func (k fieldKind) String() string {
	switch k {
	case kindBool:
		return "bool"
	case kindNumber:
		return "number"
	case kindString:
		return "string"
	case kindList:
		return "list"
	}
	return "unknown"
}

type fieldSpec struct {
	name     string
	kind     fieldKind
	ops      []string
	sortable bool
}

var rotationFieldSpecs = buildFieldSpecs()

func buildFieldSpecs() map[string]fieldSpec {
	specs := map[string]fieldSpec{}
	add := func(name string, kind fieldKind, sortable bool, ops ...string) {
		specs[name] = fieldSpec{name: name, kind: kind, ops: ops, sortable: sortable}
	}
	for _, name := range []string{"free", "deprecated", "sota", "routable", "cost_known", "reasoning", "tool_call", "structured_output"} {
		add(name, kindBool, false, "is")
	}
	for _, name := range []string{"context_window", "max_input", "max_output"} {
		add(name, kindNumber, true, ">=", ">", "<=", "<", "==", "!=")
	}
	for _, name := range []string{"cost.input", "cost.output", "cost.cache_read", "cost.cache_write"} {
		add(name, kindNumber, true, ">=", ">", "<=", "<", "==", "!=")
	}
	add("id", kindString, true, "equals", "contains", "starts_with", "ends_with", "in")
	add("protocol", kindString, true, "equals", "contains", "starts_with", "ends_with", "in")
	add("tier", kindList, false, "has_any", "has_all", "not_has")
	add("input_modalities", kindList, false, "has_any", "has_all", "not_has")
	add("output_modalities", kindList, false, "has_any", "has_all", "not_has")
	return specs
}
