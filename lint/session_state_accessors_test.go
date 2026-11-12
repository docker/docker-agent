package main

import (
	"path/filepath"
	"testing"

	"github.com/dgageot/rubocop-go/coptest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSessionStateAccessors(t *testing.T) {
	t.Parallel()

	offenses := coptest.RunProgram(t, SessionStateAccessors, coptest.ProgramFiles{
		"go.mod": "module github.com/docker/docker-agent\n\ngo 1.27\n",
		"pkg/session/session.go": `package session

type Session struct {
	AgentModelOverrides map[string]string
	CustomModelsUsed []string
	ID string
}
func (s *Session) ModelStateSnapshot() (map[string]string, []string) {
	return s.AgentModelOverrides, s.CustomModelsUsed
}
func (s *Session) AgentModelOverride(name string) (string, bool) {
	ref, ok := s.AgentModelOverrides[name]
	return ref, ok
}
func (s *Session) SetAgentModelOverride(name, ref string) { s.AgentModelOverrides[name] = ref }
func (s *Session) ReplaceModelState(m map[string]string, refs []string) {
	s.AgentModelOverrides, s.CustomModelsUsed = m, refs
}
// Fields on other types in the same package must not be confused with Session.
type Other struct { AgentModelOverrides map[string]string }
`,
		"pkg/caller/bad.go": `package caller
import s "github.com/docker/docker-agent/pkg/session"

func bad(sess *s.Session) {
	_ = sess.AgentModelOverrides["root"]
	sess.AgentModelOverrides["root"] = "model"
	delete(sess.AgentModelOverrides, "root")
	sess.CustomModelsUsed = append(sess.CustomModelsUsed, "model")
	_ = &sess.AgentModelOverrides
	consume(sess.CustomModelsUsed)
	for range sess.AgentModelOverrides {}
}
func consume(any) {}
`,
		"pkg/caller/aliases.go": `package caller
import s "github.com/docker/docker-agent/pkg/session"

type Alias = s.Session
type Pointer = *s.Session
type Defined s.Session
type Embedded struct { *Alias }
type Nested struct { Embedded }
func aliases(a *Alias, p Pointer, e *Embedded, n Nested, d *Defined) {
	_ = a.CustomModelsUsed
	_ = p.AgentModelOverrides
	_ = e.CustomModelsUsed
	_ = n.AgentModelOverrides
	_ = d.CustomModelsUsed
}
`,
		"pkg/caller/safe.go": `package caller
import s "github.com/docker/docker-agent/pkg/session"

type Other struct { AgentModelOverrides map[string]string; CustomModelsUsed []string }
type Shadow struct { *s.Session; AgentModelOverrides string }
type Methods struct{}
func (Methods) CustomModelsUsed() {}
func safe(sess *s.Session, other Other, samePackage s.Other, shadow Shadow) {
	overrides, refs := sess.ModelStateSnapshot()
	_ = overrides["root"]
	refs = append(refs, "model")
	sess.SetAgentModelOverride("root", "model")
	sess.ReplaceModelState(overrides, refs)
	_, _ = sess.AgentModelOverride("root")
	_ = sess.ID
	_ = &s.Session{AgentModelOverrides: map[string]string{"root": "model"}, CustomModelsUsed: []string{"model"}}
	_ = other.AgentModelOverrides
	_ = other.CustomModelsUsed
	_ = samePackage.AgentModelOverrides
	_ = shadow.AgentModelOverrides
	Methods{}.CustomModelsUsed()
}
`,
		// Only the actual owner package is exempt, not every package named session.
		"pkg/other/impostor.go": `package session
import s "github.com/docker/docker-agent/pkg/session"
func bad(sess *s.Session) { _ = sess.CustomModelsUsed }
`,
	})

	require.Len(t, offenses, 14)
	counts := make(map[string]int)
	for _, offense := range offenses {
		assert.Equal(t, "Lint/SessionStateAccessors", offense.CopName)
		assert.Contains(t, offense.Message, "bypasses synchronization; use")
		assert.Contains(t, offense.Message, "ModelStateSnapshot")
		counts[filepath.Base(offense.Pos.Filename)]++
		assert.NotContains(t, offense.Pos.Filename, "safe.go")
		assert.NotContains(t, offense.Pos.Filename, "session.go")
	}
	assert.Equal(t, map[string]int{"bad.go": 8, "aliases.go": 5, "impostor.go": 1}, counts)
}
