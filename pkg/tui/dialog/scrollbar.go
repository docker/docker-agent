package dialog

import (
	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/scrollview"
)

// scrollviewOwner is implemented by this package's dialogs, whose viewport the
// manager animates directly.
type scrollviewOwner interface {
	activeScrollview() *scrollview.Model
}

// scrollbarPointerOwner is implemented by reusable dialogs that own their
// viewport's scrollbar presentation; they hide it when covered or moved.
type scrollbarPointerOwner interface {
	ClearScrollbarPointer()
}

func dialogScrollview(d Dialog) *scrollview.Model {
	if owner, ok := d.(scrollviewOwner); ok {
		return owner.activeScrollview()
	}
	return nil
}

func clearDialogScrollbar(d Dialog) {
	if view := dialogScrollview(d); view != nil {
		view.ClearScrollbarPointer()
	} else if owner, ok := d.(scrollbarPointerOwner); ok {
		owner.ClearScrollbarPointer()
	}
}

// updateTopAnimation delivers a shared tick to the top dialog only; covered
// dialogs released their scrollbar subscriptions when they were covered.
// Dialogs that own animation lifecycle ([animation.Stopper]) receive the tick
// through normal forwarding.
func (d *manager) updateTopAnimation(tick animation.TickMsg) tea.Cmd {
	top := d.TopDialog()
	if view := dialogScrollview(top); view != nil {
		view.UpdateScrollbarAnimation(tick)
		return nil
	}
	if _, ok := top.(animation.Stopper); ok {
		return d.forwardToTop(tick)
	}
	return nil
}

func (d *manager) bindTopScrollbar() {
	if view := dialogScrollview(d.TopDialog()); view != nil {
		view.SetAnimationRuntime(d.clock)
	}
}

func (d *manager) ClearScrollbarPointer() {
	if top := d.TopDialog(); top != nil {
		clearDialogScrollbar(top)
	}
}

func (p *pickerCore) activeScrollview() *scrollview.Model           { return p.scrollview }
func (d *readOnlyScrollDialog) activeScrollview() *scrollview.Model { return d.scrollview }
func (d *contextDialog) activeScrollview() *scrollview.Model        { return d.scrollview }
func (d *costDialog) activeScrollview() *scrollview.Model           { return d.scrollview }
func (d *planDetailDialog) activeScrollview() *scrollview.Model     { return d.scrollview }
func (d *planBrowserDialog) activeScrollview() *scrollview.Model    { return d.scrollview }
func (d *sessionBrowserDialog) activeScrollview() *scrollview.Model { return d.scrollview }
func (d *ElicitationDialog) activeScrollview() *scrollview.Model    { return d.scrollview }
func (d *settingsDialog) activeScrollview() *scrollview.Model       { return d.body }
