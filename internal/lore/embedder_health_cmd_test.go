package lore

import (
	"strings"
	"testing"

	"github.com/mathomhaus/guild/internal/command"
	"github.com/mathomhaus/guild/internal/lore/embed"
)

func TestFormatEmbedderHealthIncludesQuestCorpus(t *testing.T) {
	body := formatEmbedderHealth(command.CLISink{}, EmbedderHealthCmdOutput{
		Report: &embed.HealthReport{
			State:       embed.EmbedderStateEnabled,
			CoverageNum: 3,
			CoverageDen: 4,
			CoveragePct: 75,
		},
		QuestReport: &embed.HealthReport{
			State:       embed.EmbedderStateEnabled,
			CoverageNum: 1,
			CoverageDen: 2,
			CoveragePct: 50,
		},
	})

	for _, want := range []string{
		"lore corpus",
		"coverage:        3/4 (75.0%)",
		"quest corpus",
		"coverage:        1/2 (50.0%)",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("health output missing %q:\n%s", want, body)
		}
	}
}
