package mermaid

import (
	"slices"
	"strings"

	"github.com/charmbracelet/x/ansi"

	mermaidparser "github.com/docker/docker-agent/pkg/mermaid"
)

func drawMermaidFlowchart(edges []mermaidparser.Edge, standalone []string, nodes map[string]string, direction string, width int, grouped ...bool) string {
	groupGap := 0
	if len(grouped) > 0 && grouped[0] {
		groupGap = 4
	}
	switch strings.ToUpper(direction) {
	case "LR":
		return drawMermaidGraphHorizontal(edges, standalone, nodes, width, groupGap)
	case "RL":
		return flipMermaidGraphHorizontal(drawMermaidGraphHorizontal(edges, standalone, nodes, width, groupGap), width, mermaidGraphLabels(edges, nodes))
	case "BT":
		return flipMermaidGraphVertical(drawMermaidGraph(edges, standalone, nodes, width))
	default: // Mermaid treats TD and TB as aliases; an omitted direction is also top-down.
		return drawMermaidGraph(edges, standalone, nodes, width)
	}
}

func drawMermaidGraphHorizontal(edges []mermaidparser.Edge, standalone []string, nodes map[string]string, width, groupGap int) string {
	adjacency, roots, order := mermaidGraph(edges, standalone)
	expanded := make(map[string]bool)
	var layouts []mermaidGraphLayout
	candidates := slices.Concat(roots, order)
	for _, root := range candidates {
		if !expanded[root] {
			layouts = append(layouts, buildMermaidHorizontalLayout(root, adjacency, nodes, expanded, groupGap))
		}
	}

	var out []string
	for _, layout := range layouts {
		if layout.width > width {
			return drawMermaidGraphLinear(edges, standalone, nodes, width)
		}
		if len(out) > 0 {
			out = append(out, "")
		}
		indent := max((width-layout.width)/2, 0)
		for _, line := range layout.lines {
			out = append(out, strings.Repeat(" ", indent)+strings.TrimRight(line, " "))
		}
	}
	return strings.Join(out, "\n")
}

func buildMermaidHorizontalLayout(id string, adjacency map[string][]mermaidparser.Edge, nodes map[string]string, expanded map[string]bool, groupGap int) mermaidGraphLayout {
	expanded[id] = true
	label := nodes[id]
	nodeWidth := max(mermaidStringWidth(label)+4, 8)
	node := boxParts(label, nodeWidth)
	edges := adjacency[id]
	if len(edges) == 0 {
		return mermaidGraphLayout{lines: node, width: nodeWidth, root: 1}
	}

	children := make([]mermaidGraphLayout, 0, len(edges))
	gap := 5 + groupGap
	for _, edge := range edges {
		gap = max(gap, mermaidStringWidth(edge.Label)+5)
		if expanded[edge.To] {
			ref := "↩ " + nodes[edge.To]
			refWidth := max(mermaidStringWidth(ref)+4, 8)
			children = append(children, mermaidGraphLayout{lines: boxParts(ref, refWidth), width: refWidth, root: 1})
		} else {
			children = append(children, buildMermaidHorizontalLayout(edge.To, adjacency, nodes, expanded, groupGap))
		}
	}

	const verticalGap = 1
	childY := make([]int, len(children))
	height := 0
	maxChildWidth := 0
	for i, child := range children {
		childY[i] = height
		height += len(child.lines)
		if i < len(children)-1 {
			height += verticalGap
		}
		maxChildWidth = max(maxChildWidth, child.width)
	}
	firstCenter := childY[0] + children[0].root
	last := len(children) - 1
	lastCenter := childY[last] + children[last].root
	root := (firstCenter + lastCenter) / 2
	shift := max(1-root, 0)
	root += shift
	height += shift
	for i := range childY {
		childY[i] += shift
	}
	height = max(height, root+2)
	layoutWidth := nodeWidth + gap + maxChildWidth
	canvas := make([][]string, height)
	for row := range canvas {
		canvas[row] = mermaidCanvas(layoutWidth)
	}
	for row, line := range node {
		writeMermaidCanvas(canvas[root-1+row], 0, line)
	}
	childX := nodeWidth + gap
	for i, child := range children {
		for row, line := range child.lines {
			writeMermaidCanvas(canvas[childY[i]+row], childX, line)
		}
		center := childY[i] + child.root
		for x := nodeWidth + 2; x < childX-1; x++ {
			canvas[center][x] = "─"
		}
		canvas[center][childX-1] = "▶"
		label := truncateMermaid(edges[i].Label, max(gap-3, 1))
		writeMermaidCanvas(canvas[center], nodeWidth+2, label)
	}
	branchX := nodeWidth + 1
	firstBranch := firstCenter + shift
	lastBranch := lastCenter + shift
	for y := min(root, firstBranch); y <= max(root, lastBranch); y++ {
		canvas[y][branchX] = "│"
	}
	for i, child := range children {
		center := childY[i] + child.root
		switch {
		case len(children) == 1:
			canvas[center][branchX] = "─"
		case i == 0:
			canvas[center][branchX] = "╭"
		case i == len(children)-1:
			canvas[center][branchX] = "╰"
		default:
			canvas[center][branchX] = "├"
		}
	}
	canvas[root][nodeWidth-1] = "├"
	for x := nodeWidth; x < branchX; x++ {
		canvas[root][x] = "─"
	}
	if len(children) > 1 {
		switch root {
		case firstBranch:
			canvas[root][branchX] = "┬"
		case lastBranch:
			canvas[root][branchX] = "┴"
		default:
			junction := "┤"
			for i, child := range children {
				if childY[i]+child.root == root {
					junction = "┼"
					break
				}
			}
			canvas[root][branchX] = junction
		}
	}

	lines := make([]string, len(canvas))
	for i := range canvas {
		lines[i] = mermaidCanvasText(canvas[i])
	}
	return mermaidGraphLayout{lines: lines, width: layoutWidth, root: root}
}

func mermaidGraphLabels(edges []mermaidparser.Edge, nodes map[string]string) []string {
	labels := make([]string, 0, len(nodes)+len(edges))
	for _, label := range nodes {
		labels = append(labels, label, "↩ "+label)
	}
	for _, edge := range edges {
		if edge.Label != "" {
			labels = append(labels, edge.Label)
		}
	}
	slices.SortFunc(labels, func(a, b string) int {
		return len(b) - len(a)
	})
	return labels
}

func flipMermaidGraphHorizontal(diagram string, width int, labels []string) string {
	lines := strings.Split(diagram, "\n")
	for i, line := range lines {
		segments := make([]string, 0, len(line)+max(width-mermaidStringWidth(line), 0))
		for line != "" {
			label := mermaidLabelPrefix(line, labels)
			if label != "" {
				segments = append(segments, label)
				line = line[len(label):]
				continue
			}
			cluster, _ := ansi.FirstGraphemeCluster(line, ansi.GraphemeWidth)
			line = line[len(cluster):]
			segments = append(segments, flipMermaidHorizontalCluster(cluster))
		}
		for range max(width-mermaidStringWidth(lines[i]), 0) {
			segments = append(segments, " ")
		}
		slices.Reverse(segments)
		lines[i] = strings.TrimRight(strings.Join(segments, ""), " ")
	}
	return strings.Join(lines, "\n")
}

func mermaidLabelPrefix(line string, labels []string) string {
	for _, label := range labels {
		if strings.HasPrefix(line, label) {
			return label
		}
	}
	return ""
}

func flipMermaidHorizontalCluster(cluster string) string {
	switch cluster {
	case "╭":
		return "╮"
	case "╮":
		return "╭"
	case "╰":
		return "╯"
	case "╯":
		return "╰"
	case "├":
		return "┤"
	case "┤":
		return "├"
	case "▶":
		return "◀"
	default:
		return cluster
	}
}

func flipMermaidGraphVertical(diagram string) string {
	lines := strings.Split(diagram, "\n")
	for i, j := 0, len(lines)-1; i <= j; i, j = i+1, j-1 {
		lines[i], lines[j] = flipMermaidVerticalLine(lines[j]), flipMermaidVerticalLine(lines[i])
	}
	return strings.Join(lines, "\n")
}

func flipMermaidVerticalLine(line string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '╭':
			return '╰'
		case '╮':
			return '╯'
		case '╰':
			return '╭'
		case '╯':
			return '╮'
		case '┬':
			return '┴'
		case '┴':
			return '┬'
		case '▼':
			return '▲'
		default:
			return r
		}
	}, line)
}

func drawMermaidGraph(edges []mermaidparser.Edge, standalone []string, nodes map[string]string, width int) string {
	adjacency, roots, order := mermaidGraph(edges, standalone)
	expanded := make(map[string]bool)
	var layouts []mermaidGraphLayout
	candidates := slices.Concat(roots, order)
	for _, root := range candidates {
		if expanded[root] {
			continue
		}
		layouts = append(layouts, buildMermaidGraphLayout(root, adjacency, nodes, expanded))
	}

	var out []string
	for _, layout := range layouts {
		if layout.width > width {
			return drawMermaidGraphLinear(edges, standalone, nodes, width)
		}
		if len(out) > 0 {
			out = append(out, "")
		}
		indent := max((width-layout.width)/2, 0)
		for _, line := range layout.lines {
			out = append(out, strings.Repeat(" ", indent)+strings.TrimRight(line, " "))
		}
	}
	return strings.Join(out, "\n")
}

type mermaidGraphLayout struct {
	lines []string
	width int
	root  int
}

func mermaidGraph(edges []mermaidparser.Edge, standalone []string) (map[string][]mermaidparser.Edge, []string, []string) {
	adjacency := make(map[string][]mermaidparser.Edge)
	indegree := make(map[string]int)
	known := make(map[string]bool)
	var order []string
	add := func(id string) {
		if !known[id] {
			known[id] = true
			order = append(order, id)
		}
	}
	for _, edge := range edges {
		add(edge.From)
		add(edge.To)
		adjacency[edge.From] = append(adjacency[edge.From], edge)
		indegree[edge.To]++
	}
	for _, id := range standalone {
		add(id)
	}
	var roots []string
	for _, id := range order {
		if indegree[id] == 0 {
			roots = append(roots, id)
		}
	}
	if len(roots) == 0 && len(order) > 0 {
		roots = append(roots, order[0])
	}
	return adjacency, roots, order
}

func buildMermaidGraphLayout(id string, adjacency map[string][]mermaidparser.Edge, nodes map[string]string, expanded map[string]bool) mermaidGraphLayout {
	expanded[id] = true
	label := nodes[id]
	nodeWidth := max(mermaidStringWidth(label)+4, 8)
	node := boxParts(label, nodeWidth)
	nodeCenter := nodeWidth / 2

	edges := adjacency[id]
	if len(edges) == 0 {
		return mermaidGraphLayout{lines: node, width: nodeWidth, root: nodeCenter}
	}
	bottom := []rune(node[2])
	bottom[nodeCenter] = '┬'
	node[2] = string(bottom)

	children := make([]mermaidGraphLayout, 0, len(edges))
	for _, edge := range edges {
		if expanded[edge.To] {
			ref := "↩ " + nodes[edge.To]
			refWidth := max(mermaidStringWidth(ref)+4, 8)
			children = append(children, mermaidGraphLayout{lines: boxParts(ref, refWidth), width: refWidth, root: refWidth / 2})
			continue
		}
		children = append(children, buildMermaidGraphLayout(edge.To, adjacency, nodes, expanded))
	}

	if len(children) == 1 {
		child := children[0]
		root := max(nodeCenter, child.root)
		layoutWidth := max(root+nodeWidth-nodeCenter, root+child.width-child.root)
		if edges[0].Label != "" {
			layoutWidth = max(layoutWidth, root+2+mermaidStringWidth(edges[0].Label))
		}
		nodeOffset, childOffset := root-nodeCenter, root-child.root
		lines := placeMermaidLines(node, nodeOffset, layoutWidth)
		connector := mermaidCanvas(layoutWidth)
		connector[root] = "│"
		if edges[0].Label != "" {
			writeMermaidCanvas(connector, root+2, edges[0].Label)
		}
		lines = append(lines, mermaidCanvasText(connector))
		arrow := mermaidCanvas(layoutWidth)
		arrow[root] = "▼"
		lines = append(lines, mermaidCanvasText(arrow))
		lines = append(lines, placeMermaidLines(child.lines, childOffset, layoutWidth)...)
		return mermaidGraphLayout{lines: lines, width: layoutWidth, root: root}
	}

	const gap = 4
	childOffsets := make([]int, len(children))
	childrenWidth := 0
	for i, child := range children {
		childOffsets[i] = childrenWidth
		childrenWidth += child.width
		if i < len(children)-1 {
			childrenWidth += gap
		}
	}
	firstCenter := childOffsets[0] + children[0].root
	last := len(children) - 1
	lastCenter := childOffsets[last] + children[last].root
	root := (firstCenter + lastCenter) / 2
	leftShift := max(nodeCenter-root, 0)
	root += leftShift
	for i := range childOffsets {
		childOffsets[i] += leftShift
	}
	layoutWidth := max(childrenWidth+leftShift, root+nodeWidth-nodeCenter)
	nodeOffset := root - nodeCenter
	lines := placeMermaidLines(node, nodeOffset, layoutWidth)

	stem := mermaidCanvas(layoutWidth)
	stem[root] = "│"
	lines = append(lines, mermaidCanvasText(stem))
	junction := mermaidCanvas(layoutWidth)
	firstCenter += leftShift
	lastCenter += leftShift
	for x := firstCenter; x <= lastCenter; x++ {
		junction[x] = "─"
	}
	junction[firstCenter], junction[lastCenter], junction[root] = "╭", "╮", "┴"
	lines = append(lines, mermaidCanvasText(junction))

	labels := mermaidCanvas(layoutWidth)
	arrows := mermaidCanvas(layoutWidth)
	for i, edge := range edges {
		center := childOffsets[i] + children[i].root
		label := truncateMermaid(edge.Label, max(children[i].width-2, 1))
		writeMermaidCanvas(labels, center-mermaidStringWidth(label)/2, label)
		arrows[center] = "▼"
	}
	lines = append(lines, mermaidCanvasText(labels), mermaidCanvasText(arrows))

	maxHeight := 0
	for _, child := range children {
		maxHeight = max(maxHeight, len(child.lines))
	}
	for row := range maxHeight {
		canvas := mermaidCanvas(layoutWidth)
		for i, child := range children {
			if row < len(child.lines) {
				writeMermaidCanvas(canvas, childOffsets[i], child.lines[row])
			}
		}
		lines = append(lines, mermaidCanvasText(canvas))
	}
	return mermaidGraphLayout{lines: lines, width: layoutWidth, root: root}
}

func placeMermaidLines(lines []string, offset, width int) []string {
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		canvas := mermaidCanvas(width)
		writeMermaidCanvas(canvas, offset, line)
		out = append(out, mermaidCanvasText(canvas))
	}
	return out
}

func drawMermaidGraphLinear(edges []mermaidparser.Edge, standalone []string, nodes map[string]string, width int) string {
	width = max(width, 8)
	adjacency := make(map[string][]mermaidparser.Edge)
	indegree := make(map[string]int)
	var order []string
	known := make(map[string]bool)
	addNode := func(id string) {
		if !known[id] {
			known[id] = true
			order = append(order, id)
		}
	}
	for _, edge := range edges {
		addNode(edge.From)
		addNode(edge.To)
		adjacency[edge.From] = append(adjacency[edge.From], edge)
		indegree[edge.To]++
	}
	for _, id := range standalone {
		addNode(id)
	}

	var roots []string
	for _, id := range order {
		if indegree[id] == 0 {
			roots = append(roots, id)
		}
	}
	if len(roots) == 0 && len(order) > 0 {
		roots = append(roots, order[0])
	}

	visited := make(map[string]bool)
	var out []string
	var renderChildren func(string, string)
	renderChildren = func(id, prefix string) {
		children := adjacency[id]
		if len(children) == 1 {
			edge := children[0]
			connector := prefix + "│"
			if edge.Label != "" {
				connector += " " + edge.Label
			}
			out = append(out, truncateMermaid(connector, width))
			if visited[edge.To] {
				out = append(out, truncateMermaid(prefix+"╰──↩ "+mermaidNodeText(nodes[edge.To]), width))
				return
			}
			out = append(out, prefix+"▼")
			visited[edge.To] = true
			out = append(out, truncateMermaid(prefix+mermaidNodeText(nodes[edge.To]), width))
			renderChildren(edge.To, prefix)
			return
		}

		for i, edge := range children {
			last := i == len(children)-1
			branch, continuation := "├─", "│ "
			if last {
				branch, continuation = "╰─", "  "
			}
			arrow := "──▶ "
			if edge.Label != "" {
				arrow = edge.Label + " ─▶ "
			}
			line := prefix + branch + " " + arrow
			if visited[edge.To] {
				line += "↩ " + mermaidNodeText(nodes[edge.To])
				out = append(out, truncateMermaid(line, width))
				continue
			}
			line += mermaidNodeText(nodes[edge.To])
			out = append(out, truncateMermaid(line, width))
			visited[edge.To] = true
			renderChildren(edge.To, prefix+continuation+" ")
		}
	}

	for _, root := range roots {
		if visited[root] {
			continue
		}
		if len(out) > 0 {
			out = append(out, "")
		}
		visited[root] = true
		out = append(out, truncateMermaid(mermaidNodeText(nodes[root]), width))
		renderChildren(root, "")
	}
	for _, id := range order {
		if visited[id] {
			continue
		}
		if len(out) > 0 {
			out = append(out, "")
		}
		visited[id] = true
		out = append(out, truncateMermaid(mermaidNodeText(nodes[id]), width))
		renderChildren(id, "")
	}
	return strings.Join(out, "\n")
}

func mermaidNodeText(label string) string {
	if strings.HasPrefix(label, "◇ ") && strings.HasSuffix(label, " ◇") {
		return label
	}
	return "[ " + label + " ]"
}
