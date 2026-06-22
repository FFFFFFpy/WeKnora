package service

import (
	"context"
	"encoding/json"
	htmlpkg "html"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/searchutil"
	"github.com/Tencent/WeKnora/internal/types"
)

const wikiImageSubjectConfidenceThreshold = 0.55

var (
	wikiMarkdownImageRe = regexp.MustCompile(`!\[([^\]]*)\]\(([^)\s]+)(?:\s+["'][^)]*["'])?\)`)
	wikiHTMLImageRe     = regexp.MustCompile(`(?is)<img\b[^>]*>`)
	wikiEnrichedImageRe = regexp.MustCompile(`(?is)<image\b[^>]*\burl\s*=\s*["']([^"']+)["'][^>]*>.*?</image>`)
	wikiCaptionRe       = regexp.MustCompile(`(?is)<image_caption>(.*?)</image_caption>`)
	wikiOCRRe           = regexp.MustCompile(`(?is)<image_ocr>(.*?)</image_ocr>`)
)

type wikiImageOccurrence struct {
	URL      string
	Alt      string
	RawStart int
	RawEnd   int
	Markup   string
}

type wikiImageDecision int

const (
	wikiImageDecisionUnknown wikiImageDecision = iota
	wikiImageDecisionKeep
	wikiImageDecisionDrop
	wikiImageDecisionConflict
)

type wikiImageDecisionEvidence struct {
	Decision wikiImageDecision
	URL      string
	Slug     string
	Reason   string
}

type wikiImageAttributionContext struct {
	KeepByURLSlug map[string]map[string]bool
}

func (c *wikiImageAttributionContext) keep(url, slug string) bool {
	if c == nil || url == "" || slug == "" {
		return true
	}
	bySlug, ok := c.KeepByURLSlug[url]
	if !ok {
		return true
	}
	keep, ok := bySlug[slug]
	if !ok {
		return true
	}
	return keep
}

func parseImageOccurrences(content string) []wikiImageOccurrence {
	if content == "" {
		return nil
	}
	type candidate struct {
		wikiImageOccurrence
		priority int
	}
	var candidates []candidate
	for _, loc := range wikiEnrichedImageRe.FindAllStringSubmatchIndex(content, -1) {
		if len(loc) < 4 || loc[2] < 0 || loc[3] < 0 {
			continue
		}
		markup := content[loc[0]:loc[1]]
		candidates = append(candidates, candidate{
			wikiImageOccurrence: wikiImageOccurrence{
				URL:      strings.TrimSpace(htmlpkg.UnescapeString(content[loc[2]:loc[3]])),
				Alt:      extractImageOriginalAlt(markup),
				RawStart: loc[0],
				RawEnd:   loc[1],
				Markup:   markup,
			},
			priority: 0,
		})
	}
	for _, loc := range wikiHTMLImageRe.FindAllStringIndex(content, -1) {
		markup := content[loc[0]:loc[1]]
		candidates = append(candidates, candidate{
			wikiImageOccurrence: wikiImageOccurrence{
				URL:      strings.TrimSpace(extractHTMLAttr(markup, "src")),
				Alt:      strings.TrimSpace(extractHTMLAttr(markup, "alt")),
				RawStart: loc[0],
				RawEnd:   loc[1],
				Markup:   markup,
			},
			priority: 1,
		})
	}
	for _, loc := range wikiMarkdownImageRe.FindAllStringSubmatchIndex(content, -1) {
		if len(loc) < 6 || loc[2] < 0 || loc[3] < 0 || loc[4] < 0 || loc[5] < 0 {
			continue
		}
		candidates = append(candidates, candidate{
			wikiImageOccurrence: wikiImageOccurrence{
				URL:      strings.TrimSpace(content[loc[4]:loc[5]]),
				Alt:      strings.TrimSpace(content[loc[2]:loc[3]]),
				RawStart: loc[0],
				RawEnd:   loc[1],
				Markup:   content[loc[0]:loc[1]],
			},
			priority: 2,
		})
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].RawStart != candidates[j].RawStart {
			return candidates[i].RawStart < candidates[j].RawStart
		}
		return candidates[i].priority < candidates[j].priority
	})
	out := make([]wikiImageOccurrence, 0, len(candidates))
	lastEnd := -1
	for _, c := range candidates {
		if c.URL == "" || c.RawStart < lastEnd || c.RawEnd > len(content) {
			continue
		}
		out = append(out, c.wikiImageOccurrence)
		lastEnd = c.RawEnd
	}
	return out
}

func enforceCitedImageAttribution(content, slug string, attribution *wikiImageAttributionContext) string {
	if content == "" || attribution == nil {
		return content
	}
	occurrences := parseImageOccurrences(content)
	if len(occurrences) == 0 {
		return content
	}
	var b strings.Builder
	last := 0
	for _, occ := range occurrences {
		if occ.RawStart < last || occ.RawEnd > len(content) {
			continue
		}
		b.WriteString(content[last:occ.RawStart])
		if attribution.keep(occ.URL, slug) {
			b.WriteString(content[occ.RawStart:occ.RawEnd])
		} else if strings.HasPrefix(strings.ToLower(strings.TrimSpace(occ.Markup)), "<image") {
			if text := extractEnrichedImageText(occ.Markup); text != "" {
				b.WriteString(text)
			}
		}
		last = occ.RawEnd
	}
	b.WriteString(content[last:])
	return collapseWikiBlankLines(b.String())
}

func (s *wikiIngestService) buildWikiImageAttributionContext(
	ctx context.Context,
	kbID string,
	tenantID uint64,
	slugUpdates map[string][]SlugUpdate,
	dropUnnamedSharedPortraits bool,
) *wikiImageAttributionContext {
	entityNamesBySlug, namesCompleteBySlug := s.buildWikiImageEntityNames(ctx, kbID, slugUpdates)
	contentByChunk, contentCompleteByChunk := s.batchReadChunkContent(ctx, tenantID, allSourceChunkIDsFrom(slugUpdates))
	rawByChunk := searchutil.CollectImageInfoByChunkIDs(ctx, s.chunkRepo, tenantID, allSourceChunkIDsFrom(slugUpdates))
	infoByChunk := decodeImageInfoByChunk(rawByChunk)

	type pendingEvidence struct {
		occ  wikiImageOccurrence
		info *types.ImageInfo
		slug string
	}
	var pending []pendingEvidence
	citingSlugsByURL := make(map[string]map[string]struct{})
	evidenceByURLSlug := make(map[string]map[string][]wikiImageDecisionEvidence)
	for slug, updates := range slugUpdates {
		for _, add := range wikiEntityConceptUpdates(updates) {
			for _, chunkID := range add.SourceChunks {
				content, ok := contentByChunk[chunkID]
				if !ok || !contentCompleteByChunk[chunkID] {
					continue
				}
				infos := infoByChunk[chunkID]
				for _, occ := range parseImageOccurrences(content) {
					if _, ok := citingSlugsByURL[occ.URL]; !ok {
						citingSlugsByURL[occ.URL] = make(map[string]struct{})
					}
					citingSlugsByURL[occ.URL][slug] = struct{}{}
					info := infos[occ.URL]
					pending = append(pending, pendingEvidence{occ: occ, info: info, slug: slug})
				}
			}
		}
	}

	for _, item := range pending {
		sharedUnnamedPortrait := dropUnnamedSharedPortraits && len(citingSlugsByURL[item.occ.URL]) >= 2
		ev := decideWikiImageAttribution(item.occ, item.info, item.slug, entityNamesBySlug[item.slug], namesCompleteBySlug[item.slug], sharedUnnamedPortrait)
		if _, ok := evidenceByURLSlug[item.occ.URL]; !ok {
			evidenceByURLSlug[item.occ.URL] = make(map[string][]wikiImageDecisionEvidence)
		}
		evidenceByURLSlug[item.occ.URL][item.slug] = append(evidenceByURLSlug[item.occ.URL][item.slug], ev)
	}

	keepByURLSlug := make(map[string]map[string]bool)
	var drops int
	for url, bySlug := range evidenceByURLSlug {
		for slug, items := range bySlug {
			decision := aggregateWikiImageDecision(items)
			if _, ok := keepByURLSlug[url]; !ok {
				keepByURLSlug[url] = make(map[string]bool)
			}
			keepByURLSlug[url][slug] = decision != wikiImageDecisionDrop
			if decision == wikiImageDecisionDrop {
				drops++
			}
		}
	}
	logger.Infof(ctx, "wiki image attribution: built decisions urls=%d drops=%d", len(keepByURLSlug), drops)
	return &wikiImageAttributionContext{KeepByURLSlug: keepByURLSlug}
}

func (s *wikiIngestService) buildWikiImageEntityNames(ctx context.Context, kbID string, slugUpdates map[string][]SlugUpdate) (map[string][]string, map[string]bool) {
	slugs := allEntityConceptSlugsFrom(slugUpdates)
	names := make(map[string][]string, len(slugs))
	complete := make(map[string]bool, len(slugs))
	for _, slug := range slugs {
		names[slug] = append(names[slug], slug)
		complete[slug] = true
		for _, add := range wikiEntityConceptUpdates(slugUpdates[slug]) {
			names[slug] = append(names[slug], add.Item.Name)
			names[slug] = append(names[slug], add.Item.Aliases...)
		}
	}
	if len(slugs) == 0 {
		return names, complete
	}
	existing, err := s.wikiService.ListBySlugs(ctx, kbID, slugs)
	if err != nil {
		logger.Warnf(ctx, "wiki image attribution: ListBySlugs(%d slugs) failed; fail-open image decisions: %v", len(slugs), err)
		for _, slug := range slugs {
			complete[slug] = false
		}
		return normalizeWikiImageNames(names), complete
	}
	for _, slug := range slugs {
		if page := existing[slug]; page != nil {
			names[slug] = append(names[slug], page.Title)
			names[slug] = append(names[slug], []string(page.Aliases)...)
		}
	}
	return normalizeWikiImageNames(names), complete
}

func (s *wikiIngestService) batchReadChunkContent(ctx context.Context, tenantID uint64, chunkIDs []string) (map[string]string, map[string]bool) {
	contentByID := make(map[string]string, len(chunkIDs))
	completeByID := make(map[string]bool, len(chunkIDs))
	for _, id := range chunkIDs {
		completeByID[id] = false
	}
	const batchSize = 500
	for start := 0; start < len(chunkIDs); start += batchSize {
		end := start + batchSize
		if end > len(chunkIDs) {
			end = len(chunkIDs)
		}
		batch := chunkIDs[start:end]
		chunks, err := s.chunkRepo.ListChunksByID(ctx, tenantID, batch)
		if err != nil {
			logger.Warnf(ctx, "wiki image attribution: failed to read %d chunk contents; fail-open affected images: %v", len(batch), err)
			continue
		}
		for _, c := range chunks {
			if c == nil {
				continue
			}
			contentByID[c.ID] = c.Content
			completeByID[c.ID] = true
		}
	}
	return contentByID, completeByID
}

func decideWikiImageAttribution(occ wikiImageOccurrence, info *types.ImageInfo, slug string, names []string, namesComplete bool, unnamedSharedPortrait bool) wikiImageDecisionEvidence {
	if !namesComplete {
		return wikiImageDecisionEvidence{Decision: wikiImageDecisionKeep, URL: occ.URL, Slug: slug, Reason: "names_incomplete"}
	}
	if info == nil || strings.TrimSpace(info.Subject) == "" || info.SubjectConfidence < wikiImageSubjectConfidenceThreshold {
		return wikiImageDecisionEvidence{Decision: wikiImageDecisionKeep, URL: occ.URL, Slug: slug, Reason: "subject_unknown"}
	}
	subject := strings.ToLower(strings.TrimSpace(info.Subject))
	ref := effectiveWikiImageRef(subject, occ, info)
	if ref != "" && wikiImageNameMatches(ref, names) {
		return wikiImageDecisionEvidence{Decision: wikiImageDecisionKeep, URL: occ.URL, Slug: slug, Reason: "subject_matches_slug"}
	}
	switch subject {
	case "person":
		if ref != "" {
			return wikiImageDecisionEvidence{Decision: wikiImageDecisionDrop, URL: occ.URL, Slug: slug, Reason: "other_person"}
		}
		if unnamedSharedPortrait {
			return wikiImageDecisionEvidence{Decision: wikiImageDecisionDrop, URL: occ.URL, Slug: slug, Reason: "unnamed_shared_person"}
		}
	case "logo":
		if ref != "" {
			return wikiImageDecisionEvidence{Decision: wikiImageDecisionDrop, URL: occ.URL, Slug: slug, Reason: "other_logo"}
		}
	case "decorative":
		return wikiImageDecisionEvidence{Decision: wikiImageDecisionDrop, URL: occ.URL, Slug: slug, Reason: "decorative"}
	case "screenshot", "diagram":
		if ref != "" {
			return wikiImageDecisionEvidence{Decision: wikiImageDecisionDrop, URL: occ.URL, Slug: slug, Reason: "other_visual_subject"}
		}
	}
	return wikiImageDecisionEvidence{Decision: wikiImageDecisionKeep, URL: occ.URL, Slug: slug, Reason: "ambiguous"}
}

func aggregateWikiImageDecision(items []wikiImageDecisionEvidence) wikiImageDecision {
	if len(items) == 0 {
		return wikiImageDecisionKeep
	}
	for _, item := range items {
		if item.Decision == wikiImageDecisionKeep || item.Decision == wikiImageDecisionUnknown || item.Decision == wikiImageDecisionConflict {
			return wikiImageDecisionKeep
		}
		if item.Decision != wikiImageDecisionDrop {
			return wikiImageDecisionKeep
		}
	}
	return wikiImageDecisionDrop
}

func effectiveWikiImageRef(subject string, occ wikiImageOccurrence, info *types.ImageInfo) string {
	alt := strings.TrimSpace(occ.Alt)
	ref := strings.TrimSpace(info.SubjectRef)
	switch subject {
	case "person":
		if looksLikePersonName(alt) {
			return alt
		}
		if captionRef := extractPersonRefFromCaption(info.Caption); captionRef != "" {
			return captionRef
		}
		if looksLikePersonName(ref) {
			return ref
		}
		return ""
	case "logo":
		if ref != "" {
			return ref
		}
		return alt
	default:
		if ref != "" {
			return ref
		}
		return alt
	}
}

func looksLikePersonName(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	lower := strings.ToLower(s)
	if containsAny(lower, "portrait", "headshot", "person", "people", "photo", "image", "logo", "screenshot", "人像", "肖像", "头像", "人物", "照片", "合影", "标志", "截图") {
		return false
	}
	var han, letters int
	for _, r := range s {
		switch {
		case unicode.Is(unicode.Han, r):
			han++
		case unicode.IsLetter(r):
			letters++
		}
	}
	if han > 0 {
		return han >= 2 && han <= 4
	}
	fields := strings.Fields(strings.ReplaceAll(s, "·", " "))
	return letters >= 4 && len(fields) >= 2
}

func extractPersonRefFromCaption(caption string) string {
	caption = strings.TrimSpace(caption)
	if caption == "" {
		return ""
	}
	for _, marker := range []string{"人像", "肖像", "头像", "照片"} {
		idx := strings.Index(caption, marker)
		if idx <= 0 {
			continue
		}
		before := strings.Trim(caption[:idx], " \t\r\n，。,.：:的")
		parts := strings.Fields(before)
		if len(parts) > 0 {
			return parts[len(parts)-1]
		}
		return before
	}
	return ""
}

func wikiImageNameMatches(ref string, names []string) bool {
	refNorm := normalizeWikiImageName(ref)
	if refNorm == "" {
		return false
	}
	for _, name := range names {
		nameNorm := normalizeWikiImageName(name)
		if nameNorm == "" {
			continue
		}
		if refNorm == nameNorm {
			return true
		}
		if runeLen(nameNorm) >= 4 && strings.Contains(refNorm, nameNorm) {
			return true
		}
		if runeLen(refNorm) >= 4 && strings.Contains(nameNorm, refNorm) {
			return true
		}
	}
	return false
}

func normalizeWikiImageNames(in map[string][]string) map[string][]string {
	out := make(map[string][]string, len(in))
	for slug, names := range in {
		seen := make(map[string]bool)
		for _, name := range names {
			n := normalizeWikiImageName(name)
			if n == "" || seen[n] {
				continue
			}
			seen[n] = true
			out[slug] = append(out[slug], n)
		}
	}
	return out
}

func normalizeWikiImageName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "entity/")
	s = strings.TrimPrefix(s, "concept/")
	s = strings.Trim(s, " \t\r\n\"'()[]{}<>《》【】（）")
	for _, suffix := range []string{"有限公司", "股份有限公司", "有限责任公司", "公司", "集团", "先生", "女士", "博士", "教授", "老师"} {
		s = strings.TrimSuffix(s, suffix)
	}
	var b strings.Builder
	for _, r := range s {
		if unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func decodeImageInfoByChunk(rawByChunk map[string]string) map[string]map[string]*types.ImageInfo {
	out := make(map[string]map[string]*types.ImageInfo, len(rawByChunk))
	for chunkID, raw := range rawByChunk {
		var infos []types.ImageInfo
		if err := json.Unmarshal([]byte(raw), &infos); err != nil {
			continue
		}
		byURL := make(map[string]*types.ImageInfo, len(infos))
		for i := range infos {
			if infos[i].URL != "" {
				byURL[infos[i].URL] = &infos[i]
			}
			if infos[i].OriginalURL != "" && infos[i].OriginalURL != infos[i].URL {
				byURL[infos[i].OriginalURL] = &infos[i]
			}
		}
		out[chunkID] = byURL
	}
	return out
}

func wikiEntityConceptUpdates(updates []SlugUpdate) []SlugUpdate {
	out := make([]SlugUpdate, 0, len(updates))
	for _, u := range updates {
		if u.Type == types.WikiPageTypeEntity || u.Type == types.WikiPageTypeConcept {
			out = append(out, u)
		}
	}
	return out
}

func allEntityConceptSlugsFrom(slugUpdates map[string][]SlugUpdate) []string {
	seen := make(map[string]bool)
	var out []string
	for slug, updates := range slugUpdates {
		for _, u := range updates {
			if u.Type != types.WikiPageTypeEntity && u.Type != types.WikiPageTypeConcept {
				continue
			}
			if !seen[slug] {
				seen[slug] = true
				out = append(out, slug)
			}
		}
	}
	sort.Strings(out)
	return out
}

func allSourceChunkIDsFrom(slugUpdates map[string][]SlugUpdate) []string {
	seen := make(map[string]bool)
	var out []string
	for _, updates := range slugUpdates {
		for _, u := range wikiEntityConceptUpdates(updates) {
			for _, id := range u.SourceChunks {
				if id == "" || seen[id] {
					continue
				}
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	sort.Strings(out)
	return out
}

func extractHTMLAttr(markup, name string) string {
	re := regexp.MustCompile(`(?is)\b` + regexp.QuoteMeta(name) + `\s*=\s*["']([^"']*)["']`)
	m := re.FindStringSubmatch(markup)
	if len(m) < 2 {
		return ""
	}
	return htmlpkg.UnescapeString(m[1])
}

func extractImageOriginalAlt(markup string) string {
	m := wikiMarkdownImageRe.FindStringSubmatch(markup)
	if len(m) >= 2 {
		return strings.TrimSpace(m[1])
	}
	return strings.TrimSpace(extractHTMLAttr(markup, "alt"))
}

func extractEnrichedImageText(markup string) string {
	var parts []string
	for _, re := range []*regexp.Regexp{wikiCaptionRe, wikiOCRRe} {
		for _, m := range re.FindAllStringSubmatch(markup, -1) {
			if len(m) < 2 {
				continue
			}
			text := strings.TrimSpace(htmlpkg.UnescapeString(m[1]))
			if text != "" {
				parts = append(parts, text)
			}
		}
	}
	return strings.Join(parts, "\n")
}

func collapseWikiBlankLines(s string) string {
	s = strings.TrimSpace(s)
	for strings.Contains(s, "\n\n\n") {
		s = strings.ReplaceAll(s, "\n\n\n", "\n\n")
	}
	return s
}

func runeLen(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
}
