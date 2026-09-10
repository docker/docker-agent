package messages

import "github.com/docker/docker-agent/pkg/plans"

// Plan management messages drive the /plans browser. Dialogs emit these
// intents; the app model services them through the pkg/plans host service and
// pushes fresh data back into the open dialogs. Dialogs never touch storage.
//
// Mutation messages carry the version that was displayed when the user chose
// the action (never nil), so every write is guarded by optimistic locking
// and a concurrent change surfaces as an actionable conflict instead of a
// silent overwrite.
type (
	// ShowPlanBrowserMsg opens the /plans browser dialog.
	ShowPlanBrowserMsg struct{}

	// RefreshPlansMsg reloads plan data into the open plan dialogs.
	RefreshPlansMsg struct{}

	// OpenPlanDetailMsg opens the detail dialog for one plan.
	OpenPlanDetailMsg struct{ Ref plans.Ref }

	// ExportPlanMsg exports a plan's content to the default file in the
	// active working directory. The app refuses to overwrite an existing
	// file.
	ExportPlanMsg struct{ Ref plans.Ref }

	// SetPlanStatusMsg sets a shared plan's free-form status.
	SetPlanStatusMsg struct {
		Ref             plans.Ref
		Status          string
		ExpectedVersion int
	}

	// DeletePlanMsg deletes a shared plan after the user confirmed the
	// named plan at the named version.
	DeletePlanMsg struct {
		Ref             plans.Ref
		ExpectedVersion int
	}

	// CreatePlanMsg creates a new shared plan by drafting its content in
	// the external $VISUAL/$EDITOR.
	CreatePlanMsg struct{ Name string }

	// EditPlanMsg edits a plan's content in the external $VISUAL/$EDITOR.
	// ExpectedVersion is the displayed version guarding the write.
	EditPlanMsg struct {
		Ref             plans.Ref
		ExpectedVersion int
	}
)
