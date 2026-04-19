package reporter

// _ "embed" is a blank import.
// Normally, importing a package you don't directly reference in code causes a compile error.
// The "_" suppresses that error while still triggering the package's "init()" side effects.
// The "embed" package works entirely through side effects — it registers the "//go:embed" directive processor with the compiler.
// You never call any function from it directly; you just need it imported so the "//go:embed dashboard.html" directive
// above "var dashboardHTML string" is recognized at build time.
// Without the "_", Go would reject the import as unused.
import (
	_ "embed"
	"encoding/json"
	"fmt"
	"html/template"
	"os"
	"path/filepath"
	"sort"

	"GoPurge/model"
)

//go:embed dashboard.html
var dashboardHTML string

// FormatHTML is the identifier for HTML dashboard output.
const FormatHTML = "html"

type htmlData struct {
	ProjectName     string
	ProjectDir      string
	GeneratedAt     string
	TotalWaste      string
	DupGroups       int
	DupFiles        int
	LargeCount      int
	UnrefCount      int
	WarnCount       int
	ChartDataJSON   template.JS
	Duplicates      []htmlDupGroup
	LargeFiles      []model.FileEntry
	Unreferenced    []model.FileEntry
	Warnings        []string
	DupWasteBytes   int64
	LargeWasteBytes int64
	UnrefWasteBytes int64
}

type htmlDupGroup struct {
	Index     int
	Hash      string
	ShortHash string
	Files     []model.FileEntry
	Waste     string
}

func writeHTML(report model.Report, path string) error {
	data := buildHTMLData(report)

	tmpl, err := template.New("report").
		Funcs(template.FuncMap{"formatBytes": formatBytesHTML}).
		Parse(dashboardHTML)
	if err != nil {
		return fmt.Errorf("html template parse: %w", err)
	}

	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create html: %w", err)
	}
	defer f.Close()

	if err := tmpl.Execute(f, data); err != nil {
		return fmt.Errorf("html template execute: %w", err)
	}
	return nil
}

func buildHTMLData(report model.Report) htmlData {
	var dupWaste, largeWaste, unrefWaste int64
	for _, g := range report.Duplicates {
		if len(g.Files) >= 2 {
			dupWaste += g.Files[0].Size * int64(len(g.Files)-1)
		}
	}
	for _, f := range report.LargeFiles {
		largeWaste += f.Size
	}
	for _, f := range report.Unreferenced {
		unrefWaste += f.Size
	}

	dupGroups := make([]htmlDupGroup, len(report.Duplicates))
	dupFileTotal := 0
	for i, g := range report.Duplicates {
		waste := int64(0)
		if len(g.Files) >= 2 {
			waste = g.Files[0].Size * int64(len(g.Files)-1)
		}
		short := g.Hash
		if len(short) > 12 {
			short = short[:12]
		}
		dupGroups[i] = htmlDupGroup{
			Index:     i + 1,
			Hash:      g.Hash,
			ShortHash: short,
			Files:     g.Files,
			Waste:     formatBytesHTML(waste),
		}
		dupFileTotal += len(g.Files)
	}

	projectName := filepath.Base(report.ProjectDir)

	return htmlData{
		ProjectName:     projectName,
		ProjectDir:      report.ProjectDir,
		GeneratedAt:     report.GeneratedAt.Format("2006-01-02 15:04:05 UTC"),
		TotalWaste:      formatBytesHTML(report.TotalWasteBytes),
		DupGroups:       len(report.Duplicates),
		DupFiles:        dupFileTotal,
		LargeCount:      len(report.LargeFiles),
		UnrefCount:      len(report.Unreferenced),
		WarnCount:       len(report.Warnings),
		ChartDataJSON:   buildChartJSON(dupWaste, largeWaste, unrefWaste, report.LargeFiles),
		Duplicates:      dupGroups,
		LargeFiles:      report.LargeFiles,
		Unreferenced:    report.Unreferenced,
		Warnings:        report.Warnings,
		DupWasteBytes:   dupWaste,
		LargeWasteBytes: largeWaste,
		UnrefWasteBytes: unrefWaste,
	}
}

// chartJSON is the data structure injected into the HTML dashboard for Chart.js.
type chartJSON struct {
	DupWaste   int64     `json:"dupWaste"`
	LargeWaste int64     `json:"largeWaste"`
	UnrefWaste int64     `json:"unrefWaste"`
	LargeFiles []barFile `json:"largeFiles"`
}

// barFile holds display data for one bar in the large-file horizontal bar chart.
type barFile struct {
	Name string  `json:"name"`
	MB   float64 `json:"mb"`
}

// buildChartJSON serialises chart data to a template.JS value so it can be
// safely embedded inside a <script> tag without HTML-escaping.
func buildChartJSON(dupWaste, largeWaste, unrefWaste int64, files []model.FileEntry) template.JS {
	sorted := make([]model.FileEntry, len(files))
	copy(sorted, files)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Size > sorted[j].Size })
	if len(sorted) > 10 {
		sorted = sorted[:10]
	}
	bars := make([]barFile, len(sorted))
	for i, f := range sorted {
		bars[i] = barFile{
			Name: filepath.Base(f.Path),
			MB:   float64(f.Size) / (1024 * 1024),
		}
	}
	data, _ := json.Marshal(chartJSON{
		DupWaste:   dupWaste,
		LargeWaste: largeWaste,
		UnrefWaste: unrefWaste,
		LargeFiles: bars,
	})
	return template.JS(data)
}

// formatBytesHTML formats a byte count as a human-readable string with units.
func formatBytesHTML(b int64) string {
	const (
		KB = 1024
		MB = 1024 * KB
		GB = 1024 * MB
	)
	switch {
	case b >= GB:
		return fmt.Sprintf("%.2f GB", float64(b)/float64(GB))
	case b >= MB:
		return fmt.Sprintf("%.1f MB", float64(b)/float64(MB))
	case b >= KB:
		return fmt.Sprintf("%.0f KB", float64(b)/float64(KB))
	default:
		return fmt.Sprintf("%d B", b)
	}
}
