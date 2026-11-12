package mermaid

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseFlowchartSubgraphs(t *testing.T) {
	t.Parallel()

	doc, err := Parse(`flowchart LR
subgraph platform[Platform]
  gateway[API Gateway]
  subgraph services[Services]
    api[API] --> db[(Database)]
  end
  gateway --> api
end
client[Client] --> gateway`)
	require.NoError(t, err)
	require.Len(t, doc.Subgraphs, 2)
	assert.Equal(t, Subgraph{ID: "platform", Label: "Platform", Nodes: []string{"gateway", "api"}}, doc.Subgraphs[0])
	assert.Equal(t, Subgraph{ID: "services", Label: "Services", ParentID: "platform", Nodes: []string{"api", "db"}}, doc.Subgraphs[1])
}

func TestParseFlowchartAssignsRepeatedNodeToNestedSubgraph(t *testing.T) {
	t.Parallel()

	doc, err := Parse(`flowchart LR
subgraph parent[Parent]
  shared[Shared]
  subgraph child[Child]
    shared --> nested[Nested]
  end
end`)
	require.NoError(t, err)
	require.Len(t, doc.Subgraphs, 2)
	assert.Equal(t, []string{"shared"}, doc.Subgraphs[0].Nodes)
	assert.Equal(t, []string{"shared", "nested"}, doc.Subgraphs[1].Nodes)
}

func TestParseFlowchartBuildsGraph(t *testing.T) {
	t.Parallel()

	doc, err := Parse(`flowchart LR
web-app["Web; App"] -->|request| api{Available?}
api -- yes --> cache[(Cache)] --> result[Done]
api -->|no| error[Error]`)
	require.NoError(t, err)
	assert.Equal(t, DiagramFlowchart, doc.Kind)
	assert.Equal(t, "LR", doc.Direction)
	assert.Equal(t, "Web; App", doc.Nodes["web-app"].Label)
	assert.Equal(t, "Available?", doc.Nodes["api"].Label)
	assert.Equal(t, ShapeDecision, doc.Nodes["api"].Shape)
	assert.Equal(t, "Cache", doc.Nodes["cache"].Label)
	require.Len(t, doc.Edges, 4)
	assert.Equal(t, Edge{From: "web-app", To: "api", Label: "request"}, doc.Edges[0])
	assert.Equal(t, Edge{From: "api", To: "cache", Label: "yes"}, doc.Edges[1])
	assert.Equal(t, Edge{From: "cache", To: "result"}, doc.Edges[2])
}

func TestParseSequenceDiagramSeparatesReturnArrowFromParticipantID(t *testing.T) {
	t.Parallel()

	doc, err := Parse(`sequenceDiagram
actor User
participant WebApp as "Web App"
participant DB as Database
User->>WebApp: Submit form
WebApp->>DB: Save
DB-->>WebApp: Saved`)
	require.NoError(t, err)
	assert.Equal(t, DiagramSequence, doc.Kind)
	assert.Equal(t, []string{"User", "WebApp", "DB"}, doc.Participants)
	assert.Equal(t, "Web App", doc.Nodes["WebApp"].Label)
	assert.Equal(t, "Database", doc.Nodes["DB"].Label)
	require.Len(t, doc.Edges, 3)
	assert.Equal(t, "DB", doc.Edges[2].From)
	assert.Equal(t, "WebApp", doc.Edges[2].To)
	assert.Equal(t, "Saved", doc.Edges[2].Label)
}

func TestParseSequenceDiagramPreservesNotesAndMessagesInTimelineOrder(t *testing.T) {
	t.Parallel()

	doc, err := Parse(`sequenceDiagram
    participant C as Client
    participant S as Server

    Note over C,S: TCP three-way handshake

    C->>S: SYN, Seq = x
    Note right of S: SYN received

    S->>C: SYN-ACK, Seq = y, Ack = x + 1
    Note left of C: Server acknowledges client's SYN

    C->>S: ACK, Seq = x + 1, Ack = y + 1
    Note over C,S: Connection established

    C->>S: Data, Seq = x + 1
    S->>C: ACK, Ack = next expected byte`)
	require.NoError(t, err)
	require.Len(t, doc.Edges, 5)
	require.Len(t, doc.SequenceEvents, 9)
	assert.Equal(t, SequenceNote, doc.SequenceEvents[0].Kind)
	assert.Equal(t, "TCP three-way handshake", doc.SequenceEvents[0].Label)
	assert.Equal(t, NoteOver, doc.SequenceEvents[0].Placement)
	assert.Equal(t, SequenceMessage, doc.SequenceEvents[1].Kind)
	assert.Equal(t, "SYN, Seq = x", doc.SequenceEvents[1].Label)
	assert.Equal(t, NoteRightOf, doc.SequenceEvents[2].Placement)
	assert.Equal(t, "Server acknowledges client's SYN", doc.SequenceEvents[4].Label)
	assert.Equal(t, NoteLeftOf, doc.SequenceEvents[4].Placement)
	assert.Equal(t, "ACK, Ack = next expected byte", doc.SequenceEvents[8].Label)
}

func TestParseSequenceDiagramWithOnlyNotes(t *testing.T) {
	t.Parallel()

	doc, err := Parse("sequenceDiagram\nparticipant A\nparticipant B\nNote over A,B: Waiting")
	require.NoError(t, err)
	require.Len(t, doc.SequenceEvents, 1)
	assert.Equal(t, SequenceNote, doc.SequenceEvents[0].Kind)
}

func TestParseStateDiagram(t *testing.T) {
	t.Parallel()

	doc, err := Parse(`stateDiagram-v2
state "Processing request" as Working
[*] --> Idle
Idle --> Working: begin
Working --> [*]: complete`)
	require.NoError(t, err)
	assert.Equal(t, DiagramState, doc.Kind)
	assert.Equal(t, "Processing request", doc.Nodes["Working"].Label)
	require.Len(t, doc.Edges, 3)
	assert.Equal(t, "Start", doc.Nodes[doc.Edges[0].From].Label)
	assert.Equal(t, "End", doc.Nodes[doc.Edges[2].To].Label)
}

func TestParseStateDiagramDirection(t *testing.T) {
	t.Parallel()

	tests := []struct {
		direction string
		want      string
	}{
		{direction: "LR", want: "LR"},
		{direction: "rl", want: "RL"},
		{direction: "TB", want: "TB"},
		{direction: "BT", want: "BT"},
	}
	for _, tt := range tests {
		t.Run(tt.direction, func(t *testing.T) {
			t.Parallel()

			doc, err := Parse("stateDiagram-v2\ndirection " + tt.direction + "\n[*] --> Idle\nIdle --> [*]")
			require.NoError(t, err)
			assert.Equal(t, tt.want, doc.Direction)
		})
	}
}

func TestSplitMermaidStatementsHonorsQuotedAndDelimitedSemicolons(t *testing.T) {
	t.Parallel()

	statements := splitMermaidStatements("flowchart TD; A[\"one; two\"] --> B; B --> C\nC --> D")
	assert.Equal(t, []string{
		"flowchart TD",
		`A["one; two"] --> B`,
		"B --> C",
		"C --> D",
	}, statements)
}

func TestParseMermaidRejectsUnsupportedOrIncompleteInput(t *testing.T) {
	t.Parallel()

	_, err := Parse("pie\n  title Pets")
	require.ErrorIs(t, err, ErrUnsupportedDiagram)
	_, err = Parse("flowchart TD\nA[unfinished")
	require.ErrorIs(t, err, ErrInvalidDiagram)
}

func TestMermaidFlowchartDirective(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		statement string
		want      bool
	}{
		{"", true},
		{" \t\n\u2003", true},
		{"direction LR", true},
		{" \tClAsSdEf\u2003highlight fill:red", true},
		{"class node highlight", true},
		{"style node fill:red", true},
		{"click node callback", true},
		{"linkStyle 0 stroke:red", true},
		{"node --> other", false},
		{"unknown direction", false},
		{"directional LR", false},
		{"\xff style", false},
	} {
		t.Run(tc.statement, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, mermaidFlowchartDirective(tc.statement))
		})
	}
}
