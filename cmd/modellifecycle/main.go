// Command modellifecycle reports catalog provider bindings an upstream has
// scheduled for retirement or stopped listing, as Slack mrkdwn on stdout
// (empty when nothing is due). Schedules come from the providers' published
// deprecation tables and model APIs; "unlisted" comes from each keyed
// provider's live model list, the only signal providers without a published
// schedule (e.g. Makora) give.
//
// Usage:
//
//	go run ./cmd/modellifecycle           # findings due an alert today
//	go run ./cmd/modellifecycle --digest  # every finding plus unreadable sources
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"golang.org/x/net/html"

	"weave-os/router/internal/providers"
	"weave-os/router/internal/router/catalog"
)

const (
	openRouterModelsURL = "https://openrouter.ai/api/v1/models"
	deepInfraModelsURL  = "https://api.deepinfra.com/models/list"
	day                 = 24 * time.Hour
)

// alertDays are the days before retirement on which the daily run alerts; the
// weekly digest covers every finding regardless.
var alertDays = map[int]bool{30: true, 14: true, 7: true, 3: true, 1: true, 0: true}

var docSources = map[string]string{
	providers.ProviderAnthropic: "https://platform.claude.com/docs/en/about-claude/model-deprecations",
	providers.ProviderOpenAI:    "https://developers.openai.com/api/docs/deprecations",
	providers.ProviderGoogle:    "https://ai.google.dev/gemini-api/docs/deprecations",
	providers.ProviderTogether:  "https://docs.together.ai/docs/deprecations",
}

// modelListURLs are the live model lists of keyed providers, read with the
// deployment key. OpenRouter's public list is read alongside its expiration
// dates. The command reads these directly rather than through the provider
// adapters, which the inference boundary reserves for dispatch.
var modelListURLs = map[string]string{
	providers.ProviderAnthropic: "https://api.anthropic.com/v1/models?limit=1000",
	providers.ProviderOpenAI:    "https://api.openai.com/v1/models",
	providers.ProviderGoogle:    "https://generativelanguage.googleapis.com/v1beta/openai/models",
	providers.ProviderFireworks: "https://api.fireworks.ai/inference/v1/models",
	providers.ProviderDeepInfra: "https://api.deepinfra.com/v1/openai/models",
	providers.ProviderMakora:    "https://inference.makora.com/v1/models",
	providers.ProviderTogether:  "https://api.together.xyz/v1/models",
	providers.ProviderXAI:       "https://api.x.ai/v1/models",
	providers.ProviderMeta:      "https://api.meta.ai/v1/models",
	providers.ProviderMiniMax:   "https://api.minimax.io/v1/models",
	providers.ProviderWafer:     "https://pass.wafer.ai/v1/models",
	providers.ProviderBedrock:   "https://bedrock-mantle.us-east-1.api.aws/v1/models",
}

var (
	dateColumn        = regexp.MustCompile(`(?i)retire|shutdown|removal|sunset|end of life`)
	replacementColumn = regexp.MustCompile(`(?i)replacement|substitute|redirects`)
	modelColumn       = regexp.MustCompile(`(?i)model|system`)
	snapshotSuffix    = regexp.MustCompile(`^(\d{4}|\d{8}|\d{4}-\d{2}-\d{2})$`)
	dateLayouts       = []string{"2006-01-02", "January 2, 2006", "Jan 2, 2006", "Jan. 2, 2006"}
	httpClient        = &http.Client{Timeout: 30 * time.Second}
)

type notice struct {
	ID          string
	Retires     time.Time
	Replacement string
}

type finding struct {
	Provider   string
	UpstreamID string
	Models     []string
	// Notice is the scheduled retirement matched to the binding; its ID may be a
	// dated snapshot of UpstreamID. Zero when only Unlisted is set.
	Notice   notice
	Unlisted bool
	Source   string
}

// bindings maps provider -> upstream model ID -> catalog model IDs.
type bindings map[string]map[string][]string

func main() {
	digest := flag.Bool("digest", false, "report every finding and unreadable source, not just today's alerts")
	flag.Parse()

	ctx := context.Background()
	b := catalogBindings()
	var findings []finding
	var failures []string
	fail := func(source string, err error) {
		failures = append(failures, fmt.Sprintf("%s: %v", source, err))
		fmt.Fprintf(os.Stderr, "%s: %v\n", source, err)
	}

	for provider, url := range docSources {
		notices, err := fetchDocNotices(ctx, url)
		if err != nil {
			fail(provider+" deprecations page", err)
			continue
		}
		findings = append(findings, scheduled(b, provider, notices, url)...)
	}
	if notices, listed, err := fetchOpenRouter(ctx); err != nil {
		fail("openrouter models", err)
	} else {
		findings = append(findings, scheduled(b, providers.ProviderOpenRouter, notices, openRouterModelsURL)...)
		findings = append(findings, unlisted(b, providers.ProviderOpenRouter, listed, openRouterModelsURL)...)
	}
	if notices, err := fetchDeepInfra(ctx); err != nil {
		fail("deepinfra models", err)
	} else {
		findings = append(findings, scheduled(b, providers.ProviderDeepInfra, notices, deepInfraModelsURL)...)
	}
	for _, provider := range sortedKeys(b) {
		key := os.Getenv(providers.APIKeyEnvVar(provider))
		listURL, ok := modelListURLs[provider]
		if key == "" || !ok {
			continue
		}
		listed, err := fetchModelList(ctx, provider, listURL, key)
		if err != nil {
			fail(provider+" /models", err)
			continue
		}
		findings = append(findings, unlisted(b, provider, listed, provider+" /models")...)
	}

	fmt.Print(report(merge(findings), failures, today(), *digest))
}

func catalogBindings() bindings {
	b := bindings{}
	for _, m := range catalog.Models {
		for _, binding := range m.Providers {
			upstream := binding.UpstreamID
			if upstream == "" {
				upstream = m.ID
			}
			if b[binding.Provider] == nil {
				b[binding.Provider] = map[string][]string{}
			}
			b[binding.Provider][upstream] = append(b[binding.Provider][upstream], m.ID)
		}
	}
	return b
}

// matches reports whether id names upstream or one of its dated snapshots
// (claude-opus-4-1-20250805, gpt-5-2025-08-07); "-0" aliases also match their
// bare snapshot (claude-opus-4-0 -> claude-opus-4-20250514).
func matches(id, upstream string) bool {
	id = strings.TrimPrefix(id, "models/")
	for _, name := range []string{upstream, strings.TrimSuffix(upstream, "-0")} {
		if id == name {
			return true
		}
		if rest, ok := strings.CutPrefix(id, name+"-"); ok && snapshotSuffix.MatchString(rest) {
			return true
		}
	}
	return false
}

func scheduled(b bindings, provider string, notices []notice, source string) []finding {
	var out []finding
	for upstream, models := range b[provider] {
		for _, n := range notices {
			if matches(n.ID, upstream) {
				out = append(out, finding{Provider: provider, UpstreamID: upstream, Models: models, Notice: n, Source: source})
			}
		}
	}
	return out
}

func unlisted(b bindings, provider string, listed []string, source string) []finding {
	var out []finding
	for upstream, models := range b[provider] {
		found := false
		for _, id := range listed {
			if matches(id, upstream) {
				found = true
				break
			}
		}
		if !found {
			out = append(out, finding{Provider: provider, UpstreamID: upstream, Models: models, Unlisted: true, Source: source})
		}
	}
	return out
}

// merge folds every finding for a binding into one: the earliest scheduled
// retirement (preferring one that names a replacement) plus whether the
// provider has stopped listing it.
func merge(findings []finding) []finding {
	merged := map[string]finding{}
	for _, f := range findings {
		key := f.Provider + "\x00" + f.UpstreamID
		cur, ok := merged[key]
		if !ok {
			merged[key] = f
			continue
		}
		cur.Unlisted = cur.Unlisted || f.Unlisted
		n, c := f.Notice.Retires, cur.Notice.Retires
		if !n.IsZero() && (c.IsZero() || n.Before(c) || n.Equal(c) && cur.Notice.Replacement == "") {
			cur.Notice, cur.Source = f.Notice, f.Source
		}
		merged[key] = cur
	}
	out := make([]finding, 0, len(merged))
	for _, f := range merged {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Notice.Retires.Equal(out[j].Notice.Retires) {
			return out[i].Notice.Retires.Before(out[j].Notice.Retires)
		}
		return out[i].Provider+out[i].UpstreamID < out[j].Provider+out[j].UpstreamID
	})
	return out
}

func report(findings []finding, failures []string, now time.Time, digest bool) string {
	var lines []string
	for _, f := range findings {
		retires := f.Notice.Retires
		days := int(retires.Sub(now) / day)
		scheduledDue := !retires.IsZero() && alertDays[days]
		if !digest && !f.Unlisted && !scheduledDue {
			continue
		}
		var status []string
		if f.Unlisted {
			status = append(status, "is no longer listed by the provider")
		}
		switch {
		case retires.IsZero():
		case days < 0:
			status = append(status, "retired "+retires.Format(time.DateOnly))
		default:
			status = append(status, fmt.Sprintf("retires %s (in %d days)", retires.Format(time.DateOnly), days))
		}
		line := fmt.Sprintf("• *%s* `%s` %s · catalog `%s`", f.Provider, f.UpstreamID, strings.Join(status, " and "), strings.Join(f.Models, "`, `"))
		if id := strings.TrimPrefix(f.Notice.ID, "models/"); id != "" && id != f.UpstreamID {
			line += fmt.Sprintf(" · notice names `%s`", id)
		}
		if f.Notice.Replacement != "" {
			line += fmt.Sprintf(" · replacement: %s", f.Notice.Replacement)
		}
		if strings.HasPrefix(f.Source, "https://") {
			line += fmt.Sprintf(" · <%s|source>", f.Source)
		}
		lines = append(lines, line)
	}
	if digest && len(failures) > 0 {
		lines = append(lines, "_Unreadable sources:_ "+strings.Join(failures, "; "))
	}
	if len(lines) == 0 {
		return ""
	}
	title := "*Provider model retirements*"
	if digest {
		title += " (weekly digest)"
	}
	return title + "\n" + strings.Join(lines, "\n") + "\n"
}

func fetchDocNotices(ctx context.Context, url string) ([]notice, error) {
	body, err := get(ctx, url, nil)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	return parseDocNotices(body)
}

// parseDocNotices reads every HTML table whose header names a model column
// and a retirement column. Only cells that are exactly a date count, so "Not sooner than ..."
// tentative dates for active models are ignored.
func parseDocNotices(r io.Reader) ([]notice, error) {
	doc, err := html.Parse(r)
	if err != nil {
		return nil, err
	}
	var notices []notice
	tables := 0
	for _, table := range elements(doc, "table") {
		rows := elements(table, "tr")
		if len(rows) == 0 {
			continue
		}
		dateCol, replCol, modelCol := -1, -1, -1
		for i, h := range cells(rows[0]) {
			switch {
			case dateCol < 0 && dateColumn.MatchString(h):
				dateCol = i
			case replCol < 0 && replacementColumn.MatchString(h):
				replCol = i
			case modelCol < 0 && modelColumn.MatchString(h):
				modelCol = i
			}
		}
		if dateCol < 0 || modelCol < 0 {
			continue
		}
		tables++
		for _, row := range rows[1:] {
			cs := cells(row)
			if max(dateCol, modelCol) >= len(cs) {
				continue
			}
			retires, ok := parseDate(cs[dateCol])
			if !ok {
				continue
			}
			replacement := ""
			if replCol >= 0 && replCol < len(cs) {
				replacement = cs[replCol]
			}
			for _, id := range strings.FieldsFunc(cs[modelCol], func(r rune) bool {
				return unicode.IsSpace(r) || strings.ContainsRune("|,()*`", r)
			}) {
				notices = append(notices, notice{id, retires, replacement})
			}
		}
	}
	if tables == 0 {
		return nil, fmt.Errorf("no table with model and retirement-date columns")
	}
	return notices, nil
}

func elements(n *html.Node, tag string) []*html.Node {
	var out []*html.Node
	for d := range n.Descendants() {
		if d.Type == html.ElementNode && d.Data == tag {
			out = append(out, d)
		}
	}
	return out
}

func cells(row *html.Node) []string {
	var out []string
	for c := row.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && (c.Data == "td" || c.Data == "th") {
			out = append(out, text(c))
		}
	}
	return out
}

func text(n *html.Node) string {
	var sb strings.Builder
	for d := range n.Descendants() {
		if d.Type == html.TextNode {
			sb.WriteString(d.Data)
			sb.WriteByte(' ')
		}
	}
	return strings.Join(strings.Fields(sb.String()), " ")
}

func parseDate(s string) (time.Time, bool) {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\u2011", "-")
	for _, layout := range dateLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func fetchModelList(ctx context.Context, provider, url, key string) ([]string, error) {
	header := http.Header{"Authorization": {"Bearer " + key}}
	if provider == providers.ProviderAnthropic {
		header = http.Header{"X-Api-Key": {key}, "Anthropic-Version": {"2023-06-01"}}
	}
	body, err := get(ctx, url, header)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	payload, err := io.ReadAll(body)
	if err != nil {
		return nil, err
	}
	ids, err := providers.ParseModelIDs(payload)
	if err == nil && len(ids) == 0 {
		err = fmt.Errorf("empty model list")
	}
	return ids, err
}

func fetchOpenRouter(ctx context.Context) ([]notice, []string, error) {
	var payload struct {
		Data []struct {
			ID             string `json:"id"`
			ExpirationDate string `json:"expiration_date"`
		} `json:"data"`
	}
	if err := getJSON(ctx, openRouterModelsURL, &payload); err != nil {
		return nil, nil, err
	}
	var notices []notice
	var listed []string
	for _, m := range payload.Data {
		listed = append(listed, m.ID)
		if retires, ok := parseDate(m.ExpirationDate); ok {
			notices = append(notices, notice{ID: m.ID, Retires: retires})
		}
	}
	if len(listed) == 0 {
		return nil, nil, fmt.Errorf("empty model list")
	}
	return notices, listed, nil
}

func fetchDeepInfra(ctx context.Context) ([]notice, error) {
	var models []struct {
		ModelName  string  `json:"model_name"`
		Deprecated *int64  `json:"deprecated"`
		ReplacedBy *string `json:"replaced_by"`
	}
	if err := getJSON(ctx, deepInfraModelsURL, &models); err != nil {
		return nil, err
	}
	var notices []notice
	for _, m := range models {
		if m.Deprecated == nil {
			continue
		}
		n := notice{ID: m.ModelName, Retires: time.Unix(*m.Deprecated, 0).UTC().Truncate(day)}
		if m.ReplacedBy != nil {
			n.Replacement = *m.ReplacedBy
		}
		notices = append(notices, n)
	}
	return notices, nil
}

func getJSON(ctx context.Context, url string, v any) error {
	body, err := get(ctx, url, nil)
	if err != nil {
		return err
	}
	defer body.Close()
	return json.NewDecoder(body).Decode(v)
}

func get(ctx context.Context, url string, header http.Header) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if header != nil {
		req.Header = header
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; WeaveRouterModelLifecycle/1.0)")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	return resp.Body, nil
}

func today() time.Time { return time.Now().UTC().Truncate(day) }

func sortedKeys(b bindings) []string {
	keys := make([]string, 0, len(b))
	for k := range b {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
