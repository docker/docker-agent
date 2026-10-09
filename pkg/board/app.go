package board

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/docker/docker-agent/pkg/userconfig"
)

// DefaultAgent is the agent ref used for projects that do not set one.
const DefaultAgent = "default"

// Project is a repository cards can be created against, configured in the
// user's global config file (or through the TUI, which persists there).
type Project struct {
	Name  string
	Path  string
	Agent string
}

// App is the board engine: it owns the cards, the per-card agent sessions,
// and the configuration (projects and columns) stored in the user's global
// config file.
type App struct {
	// ctx is the board-lifetime context used by engine operations that
	// outlive a single UI interaction (git commands, tmux attach).
	ctx        context.Context //nolint:containedctx // board-lifetime context
	store      *Store
	sessions   sessionManager
	controller *controller

	// mu guards config (the mutable projects/columns section of the user's
	// global config file).
	mu      sync.Mutex
	config  *userconfig.Config
	columns []Column

	onChanged func()
}

// NewApp loads the board state and reattaches to any sessions still running
// in tmux. onChanged is called (from arbitrary goroutines) whenever a card
// changes, so the UI can refresh.
func NewApp(ctx context.Context, onChanged func()) (*App, error) {
	if _, err := exec.LookPath("tmux"); err != nil {
		return nil, errors.New("the board runs each agent in a tmux session: please install tmux first")
	}

	// One board per state file: a second instance would run its own
	// watchers and race this one relaunching agents.
	if err := acquireLock(StatePath() + ".lock"); err != nil {
		return nil, err
	}

	cfg, err := userconfig.Load()
	if err != nil {
		return nil, fmt.Errorf("load user config: %w", err)
	}

	store, err := OpenStore(StatePath())
	if err != nil {
		return nil, err
	}

	var columns []userconfig.BoardColumn
	if cfg.Board != nil {
		columns = cfg.Board.Columns
	}

	app := &App{
		ctx:       ctx,
		store:     store,
		sessions:  tmuxSessions{ctx: ctx},
		config:    cfg,
		columns:   ColumnsFromConfig(columns),
		onChanged: onChanged,
	}
	app.controller = newController(ctx, store, app.sessions, onChanged)
	app.controller.ReconcileAll()
	return app, nil
}

// columnIndexLocked returns the position of the column with the given id,
// or -1. Callers must hold a.mu.
func (a *App) columnIndexLocked(id string) int {
	return slices.IndexFunc(a.columns, func(c Column) bool { return c.ID == id })
}

// Columns returns the board's pipeline.
func (a *App) Columns() []Column {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.columns)
}

// SetColumnPrompt updates a column's prompt and persists it to the user's
// global config file.
func (a *App) SetColumnPrompt(colID, prompt string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	i := a.columnIndexLocked(colID)
	if i < 0 {
		return fmt.Errorf("unknown column %q", colID)
	}
	a.columns[i].Prompt = prompt
	return a.saveConfigLocked()
}

// AddColumn validates and appends a column to the pipeline, persisting it
// to the user's global config file. The column's id is derived from its
// name and kept unique; the saved column (with its id) is returned so the
// UI can select it.
func (a *App) AddColumn(col Column) (Column, error) {
	col, err := normalizeColumn(col)
	if err != nil {
		return Column{}, err
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	col.ID = a.uniqueColumnIDLocked(col.Name)
	a.columns = append(a.columns, col)
	if err := a.saveConfigLocked(); err != nil {
		return Column{}, err
	}
	return col, nil
}

// UpdateColumn replaces the column with the given id, applying the same
// validation as AddColumn and persisting the change. The id never changes
// on an update, so existing cards stay attached to the column across a
// rename.
func (a *App) UpdateColumn(id string, col Column) (Column, error) {
	col, err := normalizeColumn(col)
	if err != nil {
		return Column{}, err
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	idx := a.columnIndexLocked(id)
	if idx < 0 {
		return Column{}, fmt.Errorf("unknown column %q", id)
	}
	col.ID = id
	a.columns[idx] = col
	if err := a.saveConfigLocked(); err != nil {
		return Column{}, err
	}
	return col, nil
}

// MoveColumn moves the column with the given id delta positions in the
// pipeline (negative moves it left) and persists the new order. The
// destination is clamped, so moves past either end are safe no-ops.
func (a *App) MoveColumn(id string, delta int) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	idx := a.columnIndexLocked(id)
	if idx < 0 {
		return fmt.Errorf("unknown column %q", id)
	}
	dst := min(max(idx+delta, 0), len(a.columns)-1)
	if dst == idx {
		return nil
	}
	col := a.columns[idx]
	a.columns = slices.Insert(slices.Delete(a.columns, idx, idx+1), dst, col)
	return a.saveConfigLocked()
}

// RemoveColumn deletes a column by id and persists the change. A column
// that still holds cards cannot be removed (its cards would silently pile
// up in the first column), and neither can the last remaining column. The
// cards check is advisory: a card creation or move in flight can still
// land in the removed column, where the UI rescues it into the first one.
func (a *App) RemoveColumn(id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	idx := a.columnIndexLocked(id)
	if idx < 0 {
		return fmt.Errorf("unknown column %q", id)
	}
	if len(a.columns) == 1 {
		return errors.New("the board needs at least one column")
	}
	for _, card := range a.store.ListCards() {
		if card.Column == id {
			return fmt.Errorf("column %q still has cards; move or delete them first", a.columns[idx].Name)
		}
	}
	a.columns = slices.Delete(a.columns, idx, idx+1)
	return a.saveConfigLocked()
}

// normalizeColumn checks a column's name and normalizes its fields: name
// and emoji are single-line display strings (inner whitespace collapses to
// spaces), the prompt keeps inner newlines — multi-line prompts are the
// norm.
func normalizeColumn(col Column) (Column, error) {
	col.Name = collapseSpace(col.Name)
	if col.Name == "" {
		return Column{}, errors.New("column name is required")
	}
	col.Emoji = collapseSpace(col.Emoji)
	col.Prompt = strings.TrimSpace(col.Prompt)
	return col, nil
}

// uniqueColumnIDLocked derives an id from a column name, suffixing it until
// it collides with no existing column. Callers must hold a.mu.
func (a *App) uniqueColumnIDLocked(name string) string {
	base := columnID(name)
	if base == "" {
		base = fallbackColumnID(name)
	}
	id := base
	for n := 2; slices.ContainsFunc(a.columns, func(c Column) bool { return c.ID == id }); n++ {
		id = fmt.Sprintf("%s-%d", base, n)
	}
	return id
}

// saveConfigLocked persists the projects and columns to the global config
// file. Callers must hold a.mu. The board section is written from the
// board's in-memory view (authoritative for projects and columns) onto the
// freshest on-disk config, so changes other processes made to unrelated
// sections while the board was open are never overwritten; a.config is then
// swapped to that fresh copy.
func (a *App) saveConfigLocked() error {
	board := &userconfig.Board{}
	if a.config.Board != nil {
		*board = *a.config.Board
	}
	if slices.Equal(a.columns, DefaultColumns) {
		// Keep the config free of the built-in pipeline so future changes to
		// the defaults reach users who never customized their columns.
		board.Columns = nil
	} else {
		board.Columns = make([]userconfig.BoardColumn, 0, len(a.columns))
		for _, c := range a.columns {
			board.Columns = append(board.Columns, userconfig.BoardColumn{ID: c.ID, Name: c.Name, Emoji: c.Emoji, Prompt: c.Prompt})
		}
	}
	return userconfig.Update(func(cfg *userconfig.Config) error {
		cfg.Board = board
		a.config = cfg
		return nil
	})
}

// projectsLocked returns the stored projects, nil when the board section
// does not exist yet. Callers must hold a.mu.
func (a *App) projectsLocked() []userconfig.BoardProject {
	if a.config.Board == nil {
		return nil
	}
	return a.config.Board.Projects
}

// Projects returns the configured projects.
func (a *App) Projects() []Project {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.config.Board == nil {
		return nil
	}
	stored := a.projectsLocked()
	projects := make([]Project, 0, len(stored))
	for _, p := range stored {
		projects = append(projects, Project{Name: p.Name, Path: expandHome(p.Path), Agent: p.Agent})
	}
	return projects
}

// AddProject validates and appends a project, persisting it to the user's
// global config file. The path is normalized to an absolute path (expanding
// a leading ~) so cards never depend on the board's working directory.
func (a *App) AddProject(p Project) error {
	stored, err := a.validateProject(p)
	if err != nil {
		return err
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	for _, existing := range a.projectsLocked() {
		if existing.Name == stored.Name {
			return fmt.Errorf("project %q already exists", stored.Name)
		}
	}
	if a.config.Board == nil {
		a.config.Board = &userconfig.Board{}
	}
	a.config.Board.Projects = append(a.config.Board.Projects, stored)
	return a.saveConfigLocked()
}

// UpdateProject replaces the project named oldName with p, applying the
// same validation as AddProject and persisting the change. Cards created
// against the old name follow a rename; they keep the repo path and agent
// they were created with.
func (a *App) UpdateProject(oldName string, p Project) error {
	stored, err := a.validateProject(p)
	if err != nil {
		return err
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	projects := a.projectsLocked()
	idx := slices.IndexFunc(projects, func(bp userconfig.BoardProject) bool { return bp.Name == oldName })
	if idx < 0 {
		return fmt.Errorf("unknown project %q", oldName)
	}
	if stored.Name != oldName {
		for _, existing := range projects {
			if existing.Name == stored.Name {
				return fmt.Errorf("project %q already exists", stored.Name)
			}
		}
	}
	projects[idx] = stored
	if err := a.saveConfigLocked(); err != nil {
		return err
	}
	if stored.Name != oldName {
		// Keep existing cards attached to their project across the rename
		// (their accent color and header stay consistent).
		if err := a.store.RenameProject(oldName, stored.Name); err != nil {
			return err
		}
		a.onChanged()
	}
	return nil
}

// validateProject checks a project's name and path and returns its stored
// form. The name is trimmed and the path is stored ~-contracted so the
// shared config works across environments whose home differs (host vs.
// docker sandbox).
func (a *App) validateProject(p Project) (userconfig.BoardProject, error) {
	name := strings.TrimSpace(p.Name)
	if name == "" {
		return userconfig.BoardProject{}, errors.New("project name is required")
	}
	path, err := normalizeProjectPath(p.Path)
	if err != nil {
		return userconfig.BoardProject{}, err
	}
	if !isGitRepo(a.ctx, path) {
		return userconfig.BoardProject{}, fmt.Errorf("%s is not a git repository", path)
	}
	return userconfig.BoardProject{Name: name, Path: contractHome(path), Agent: strings.TrimSpace(p.Agent)}, nil
}

// normalizeProjectPath expands a leading ~ and makes the path absolute. An
// empty path is rejected: it would silently validate against the board's
// working directory.
func normalizeProjectPath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", errors.New("project path is required")
	}
	abs, err := filepath.Abs(expandHome(path))
	if err != nil {
		return "", fmt.Errorf("resolve project path: %w", err)
	}
	return abs, nil
}

// MoveProject moves the named project delta positions in the list
// (negative moves it up) and persists the new order. The destination is
// clamped to the list, so moves past either end are safe no-ops. Project
// order drives the accent colors and the new-card selector.
func (a *App) MoveProject(name string, delta int) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	projects := a.projectsLocked()
	idx := slices.IndexFunc(projects, func(p userconfig.BoardProject) bool { return p.Name == name })
	if idx < 0 {
		return fmt.Errorf("unknown project %q", name)
	}
	dst := min(max(idx+delta, 0), len(projects)-1)
	if dst == idx {
		return nil
	}
	p := projects[idx]
	projects = slices.Delete(projects, idx, idx+1)
	a.config.Board.Projects = slices.Insert(projects, dst, p)
	return a.saveConfigLocked()
}

// RemoveProject deletes a project by name and persists the change. Existing
// cards keep the repo path and agent they were created with.
func (a *App) RemoveProject(name string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.config.Board == nil {
		return nil
	}
	a.config.Board.Projects = slices.DeleteFunc(a.config.Board.Projects, func(p userconfig.BoardProject) bool {
		return p.Name == name
	})
	return a.saveConfigLocked()
}

// Cards returns all cards in board order.
func (a *App) Cards() []*Card {
	return a.store.ListCards()
}

// CreateCard creates a card in the first column and launches its agent
// session. docker agent creates the isolated git worktree (named after the
// card) on first launch and exposes its control plane on a per-card unix
// socket; the board records where the worktree lives and starts watching
// the session. The title is a placeholder derived from the prompt, replaced
// when the agent emits its session_title event, so card creation is instant.
func (a *App) CreateCard(project Project, prompt string) (card *Card, err error) {
	if strings.TrimSpace(prompt) == "" {
		return nil, errors.New("prompt is required")
	}
	agent := project.Agent
	if agent == "" {
		agent = DefaultAgent
	}

	// Checked before launch because tmux silently falls back to $HOME when
	// its start directory is missing, which would surface as a confusing
	// worktree error from the agent instead of this one.
	if !isGitRepo(a.ctx, project.Path) {
		return nil, fmt.Errorf("project path %s is not a git repository in this environment; re-add the project here", project.Path)
	}

	worktreeName := newWorktreeName()
	branch := worktreeBranch(worktreeName)
	wtPath := worktreeDir(worktreeName)
	sessionName := newSessionName()
	agentSession := newID()

	defer func() {
		if err != nil {
			_ = a.sessions.KillSession(sessionName)
			removeWorktree(a.ctx, project.Path, wtPath, branch)
		}
	}()

	// Launch from the repository: --worktree branches the new worktree from
	// the repo's upstream base (detected, not assumed).
	base := upstreamBase(a.ctx, project.Path)
	if err := a.sessions.NewSession(sessionName, project.Path, agent, agentSession, socketPath(agentSession), worktreeName, base, prompt); err != nil {
		return nil, fmt.Errorf("tmux session: %w", err)
	}

	firstColumn := a.Columns()[0].ID
	card = &Card{
		ID:           newID(),
		Title:        placeholderTitle(prompt),
		Column:       firstColumn,
		Status:       StatusStarting,
		Project:      project.Name,
		Agent:        agent,
		RepoPath:     project.Path,
		Branch:       branch,
		Worktree:     wtPath,
		Session:      sessionName,
		AgentSession: agentSession,
	}

	if err := a.store.InsertCard(card); err != nil {
		return nil, fmt.Errorf("insert card: %w", err)
	}

	a.controller.ExpectTurn(card.ID)
	a.controller.Start(card)
	a.onChanged()
	return card, nil
}

// MoveCard moves a card to the given column. A move never changes the
// card's status: the status tracks the agent's activity, not the move. A
// busy card cannot move forward; the check is enforced atomically by the
// store so a watcher flipping the status concurrently cannot slip past it.
// Moving forward sends the destination column's prompt to the card's agent;
// the move stays observable even when the prompt cannot be delivered.
func (a *App) MoveCard(cardID, colID string) error {
	card, err := a.store.GetCard(cardID)
	if err != nil {
		return err
	}

	// One pipeline snapshot for the whole move: columns can be edited
	// concurrently from the UI, and re-indexing a fresh snapshot later
	// could deliver another column's prompt (or panic on a removed one).
	columns := a.Columns()
	dstIdx := slices.IndexFunc(columns, func(c Column) bool { return c.ID == colID })
	if dstIdx < 0 {
		return fmt.Errorf("unknown column %q", colID)
	}
	srcIdx := slices.IndexFunc(columns, func(c Column) bool { return c.ID == card.Column })
	movedForward := dstIdx > srcIdx

	moved, err := a.store.MoveCard(cardID, colID, movedForward)
	if err != nil {
		return err
	}
	a.controller.Start(moved) // no-op if already watching
	a.onChanged()

	if movedForward {
		return a.controller.SendPrompt(moved, columns[dstIdx].Prompt)
	}
	return nil
}

// DeleteCard removes a card, kills its session, and cleans up its worktree.
func (a *App) DeleteCard(cardID string) error {
	card, err := a.store.GetCard(cardID)
	if err != nil {
		return err
	}
	// Remove from the store first: combined with the controller's relaunch
	// lock (Teardown), this guarantees no in-flight relaunch resurrects the
	// session after it is killed here.
	if err := a.store.DeleteCard(cardID); err != nil {
		return err
	}
	a.controller.Stop(cardID)
	a.controller.Teardown(card)
	removeWorktree(a.ctx, card.RepoPath, card.Worktree, card.Branch)
	// The agent is gone for good: drop its control-plane socket file too.
	_ = os.Remove(socketPath(card.AgentSession))
	a.onChanged()
	return nil
}

// Diff returns the card's full worktree diff against the upstream base.
func (a *App) Diff(cardID string) (string, error) {
	card, err := a.store.GetCard(cardID)
	if err != nil {
		return "", err
	}
	return worktreeDiff(a.ctx, card.Worktree)
}

// OpenEditor opens the card's worktree in the user's GUI editor: the
// command named by $DOCKER_AGENT_BOARD_EDITOR, defaulting to VS Code's `code`. The
// editor is started detached and reaped in the background.
func (a *App) OpenEditor(cardID string) error {
	card, err := a.store.GetCard(cardID)
	if err != nil {
		return err
	}
	// BOARD_EDITOR is the legacy name, kept as a fallback for one release.
	editor := cmp.Or(os.Getenv("DOCKER_AGENT_BOARD_EDITOR"), os.Getenv("BOARD_EDITOR"), "code")
	cmd := exec.CommandContext(a.ctx, editor, card.Worktree)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("open editor (%s): %w", editor, err)
	}
	// Reap the editor when it exits so it does not linger as a zombie.
	go func() { _ = cmd.Wait() }()
	return nil
}

// ErrAgentStarting means the card's agent has not answered on its control
// plane yet, so there is no UI worth attaching to.
var ErrAgentStarting = errors.New("the agent is still starting")

// AttachCommand returns the command that attaches the caller's terminal to
// the card's agent session. It fails with [ErrAgentStarting] until the
// agent's control plane answers, so the user never lands on a bare launch
// command. An errored card is attachable regardless: its agent may have
// died at startup, and the dead pane holds the error output the user needs
// to read — unless the session is gone entirely (its recreation failed), in
// which case the recorded relaunch failure, when known, beats tmux's raw
// "can't find session". Before attaching, the session's header row is
// refreshed with the card's current title and project.
func (a *App) AttachCommand(cardID string) (*exec.Cmd, error) {
	card, err := a.store.GetCard(cardID)
	if err != nil {
		return nil, err
	}
	if card.Status == StatusError {
		if exists, err := a.sessions.Exists(card.Session); err == nil && !exists {
			if lerr := a.controller.LaunchError(cardID); lerr != nil {
				return nil, fmt.Errorf("the agent could not be relaunched (%w); move the card forward to retry, or delete it", lerr)
			}
			return nil, errors.New("the agent's session is gone; move the card forward to relaunch it, or delete it")
		}
	} else if !a.controller.Ready(card) {
		return nil, ErrAgentStarting
	}
	socket, err := TmuxSocketPath()
	if err != nil {
		return nil, err
	}
	setSessionHeader(a.ctx, card.Session, card.Title, card.Project, card.Worktree)
	return exec.CommandContext(a.ctx, "tmux", "-S", socket, "attach", "-t", "="+card.Session), nil
}
