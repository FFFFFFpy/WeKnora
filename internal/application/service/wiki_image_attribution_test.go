package service

import (
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
)

func TestParseImageOccurrences_ThreeForms(t *testing.T) {
	content := `A ![唐保彪](u1)
<img alt="Kabam logo" src="u2">
<image url="u3">
<image_original>![orig](u3)</image_original>
<image_caption>caption</image_caption>
</image>`
	got := parseImageOccurrences(content)
	if len(got) != 3 {
		t.Fatalf("occurrences = %d, want 3: %+v", len(got), got)
	}
	if got[0].URL != "u1" || got[0].Alt != "唐保彪" {
		t.Fatalf("markdown occurrence wrong: %+v", got[0])
	}
	if got[1].URL != "u2" || got[1].Alt != "Kabam logo" {
		t.Fatalf("html occurrence wrong: %+v", got[1])
	}
	if got[2].URL != "u3" || got[2].Alt != "orig" {
		t.Fatalf("enriched occurrence wrong: %+v", got[2])
	}
}

func TestEnforceCitedImageAttribution_RebuildsByIndex(t *testing.T) {
	content := `intro ![唐保彪](u1) keep ![Kabam截图](u2) outro`
	ctx := &wikiImageAttributionContext{KeepByURLSlug: map[string]map[string]bool{
		"u1": {"entity/kabam": false},
		"u2": {"entity/kabam": true},
	}}
	got := enforceCitedImageAttribution(content, "entity/kabam", ctx)
	if strings.Contains(got, "u1") {
		t.Fatalf("dropped URL still present: %s", got)
	}
	if !strings.Contains(got, "![Kabam截图](u2)") || !strings.Contains(got, "intro") || !strings.Contains(got, "outro") {
		t.Fatalf("kept content missing: %s", got)
	}
}

func TestStripImageMarkup_PreservesEnrichedText(t *testing.T) {
	content := `before
<image url="u1">
<image_original>![p](u1)</image_original>
<image_caption>唐保彪人像</image_caption>
<image_ocr>Kabam</image_ocr>
</image>
after ![drop](u2)`
	got := stripImageMarkup(content)
	if strings.Contains(got, "u1") || strings.Contains(got, "u2") || strings.Contains(got, "<image") {
		t.Fatalf("image markup not stripped: %s", got)
	}
	if !strings.Contains(got, "唐保彪人像") || !strings.Contains(got, "Kabam") {
		t.Fatalf("caption/ocr not preserved: %s", got)
	}
}

func TestWikiImageAttribution_AltPersonWinsOverOCRBackground(t *testing.T) {
	occ := wikiImageOccurrence{URL: "u1", Alt: "唐保彪"}
	info := &types.ImageInfo{URL: "u1", Subject: "person", SubjectRef: "Kabam", SubjectConfidence: 0.9}
	kabam := decideWikiImageAttribution(occ, info, "entity/kabam", []string{"kabam"}, true, false)
	if kabam.Decision != wikiImageDecisionDrop {
		t.Fatalf("kabam decision = %v, want drop", kabam.Decision)
	}
	tang := decideWikiImageAttribution(occ, info, "entity/tang-bao-biao", []string{"唐保彪"}, true, false)
	if tang.Decision != wikiImageDecisionKeep {
		t.Fatalf("tang decision = %v, want keep", tang.Decision)
	}
}

func TestWikiImageAttribution_GenericPersonAltKeeps(t *testing.T) {
	occ := wikiImageOccurrence{URL: "u1", Alt: "portrait"}
	info := &types.ImageInfo{URL: "u1", Subject: "person", SubjectConfidence: 0.9}
	got := decideWikiImageAttribution(occ, info, "entity/acme", []string{"acme"}, true, false)
	if got.Decision != wikiImageDecisionKeep {
		t.Fatalf("generic alt decision = %v, want keep", got.Decision)
	}
}

func TestWikiImageAttribution_FailOpenAndConservativeAggregation(t *testing.T) {
	occ := wikiImageOccurrence{URL: "u1", Alt: "Other Person"}
	info := &types.ImageInfo{URL: "u1", Subject: "person", SubjectRef: "Other Person", SubjectConfidence: 0.9}
	ev := decideWikiImageAttribution(occ, info, "entity/acme", []string{"acme"}, false, false)
	if ev.Decision != wikiImageDecisionKeep {
		t.Fatalf("names-incomplete decision = %v, want keep", ev.Decision)
	}
	got := aggregateWikiImageDecision([]wikiImageDecisionEvidence{
		{Decision: wikiImageDecisionDrop},
		{Decision: wikiImageDecisionKeep},
	})
	if got != wikiImageDecisionKeep {
		t.Fatalf("aggregate = %v, want keep", got)
	}
}

func TestUnnamedSharedPortraitSwitch(t *testing.T) {
	occ := wikiImageOccurrence{URL: "u1"}
	info := &types.ImageInfo{URL: "u1", Subject: "person", SubjectConfidence: 0.9}
	off := decideWikiImageAttribution(occ, info, "entity/acme", []string{"acme"}, true, false)
	if off.Decision != wikiImageDecisionKeep {
		t.Fatalf("switch off decision = %v, want keep", off.Decision)
	}
	on := decideWikiImageAttribution(occ, info, "entity/acme", []string{"acme"}, true, true)
	if on.Decision != wikiImageDecisionDrop {
		t.Fatalf("switch on decision = %v, want drop", on.Decision)
	}
}
