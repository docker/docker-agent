package mermaid

import (
	"strings"

	"github.com/charmbracelet/x/ansi"

	mermaidparser "github.com/docker/docker-agent/pkg/mermaid"
)

func mermaidCanvas(width int) []string {
	canvas := make([]string, width)
	for i := range canvas {
		canvas[i] = " "
	}
	return canvas
}

func mermaidStringWidth(value string) int {
	return ansi.StringWidth(value)
}

func writeMermaidCanvas(canvas []string, start int, value string) {
	for value != "" {
		cluster, clusterWidth := ansi.FirstGraphemeCluster(value, ansi.GraphemeWidth)
		value = value[len(cluster):]
		if clusterWidth == 0 {
			for i := min(start-1, len(canvas)-1); i >= 0; i-- {
				if canvas[i] != "" {
					canvas[i] += cluster
					break
				}
			}
			continue
		}
		if start >= 0 && start < len(canvas) {
			canvas[start] = cluster
		}
		for i := 1; i < clusterWidth && start+i >= 0 && start+i < len(canvas); i++ {
			canvas[start+i] = "" // Continuation cells must not add width when joined.
		}
		start += clusterWidth
	}
}

func mermaidCanvasText(canvas []string) string {
	return strings.TrimRight(strings.Join(canvas, ""), " ")
}

func mermaidLifeline(order []string, centers map[string]int, width int) string {
	return mermaidCanvasText(sequenceCanvas(order, centers, width))
}

func drawMermaidEdges(edges []mermaidparser.Edge, standalone []string, nodes map[string]string, width int) string {
	width = max(width, 8)
	var out []string
	seen := make(map[string]bool)
	for _, edge := range edges {
		from, to := nodes[edge.From], nodes[edge.To]
		connector := " ──▶ "
		if edge.Label != "" {
			connector = " ─" + truncateMermaid(edge.Label, max(width/3, 3)) + "─▶ "
		}
		connectorWidth := mermaidStringWidth(connector)
		leftWidth := min(max(mermaidStringWidth(from)+4, 8), max((width-connectorWidth)/2, 4))
		rightWidth := min(max(mermaidStringWidth(to)+4, 8), max(width-connectorWidth-leftWidth, 4))
		if leftWidth+connectorWidth+rightWidth <= width && leftWidth >= 8 && rightWidth >= 8 {
			out = append(out, drawMermaidRow(from, to, connector, leftWidth, rightWidth)...)
		} else {
			out = append(out, drawMermaidBox(from, width)...)
			arrow := "    │"
			if edge.Label != "" {
				arrow += " " + truncateMermaid(edge.Label, max(width-7, 1))
			}
			out = append(out, arrow, "    ▼")
			out = append(out, drawMermaidBox(to, width)...)
		}
		seen[edge.From], seen[edge.To] = true, true
	}
	for _, id := range standalone {
		if !seen[id] {
			out = append(out, drawMermaidBox(nodes[id], width)...)
		}
	}
	return strings.Join(out, "\n")
}

func drawMermaidRow(from, to, connector string, leftWidth, rightWidth int) []string {
	left, right := boxParts(from, leftWidth), boxParts(to, rightWidth)
	gap := strings.Repeat(" ", mermaidStringWidth(connector))
	return []string{left[0] + gap + right[0], left[1] + connector + right[1], left[2] + gap + right[2]}
}

func drawMermaidBox(label string, width int) []string {
	boxWidth := min(max(mermaidStringWidth(label)+4, 8), width)
	return boxParts(label, boxWidth)
}

func boxParts(label string, width int) []string {
	width = max(width, 4)
	inner := width - 2
	label = truncateMermaid(label, max(inner-2, 1))
	padding := max(inner-mermaidStringWidth(label)-2, 0)
	return []string{
		"╭" + strings.Repeat("─", inner) + "╮",
		"│ " + label + strings.Repeat(" ", padding) + " │",
		"╰" + strings.Repeat("─", inner) + "╯",
	}
}

func truncateMermaid(value string, width int) string {
	if mermaidStringWidth(value) <= width {
		return value
	}
	if width <= 1 {
		return "…"
	}
	return ansi.Truncate(value, width, "…")
}
