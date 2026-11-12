package dialog

import "github.com/docker/docker-agent/pkg/tui/dialog/common"

type (
	MultiChoiceOption    = common.MultiChoiceOption
	MultiChoiceResult    = common.MultiChoiceResult
	MultiChoiceResultMsg = common.MultiChoiceResultMsg
	MultiChoiceConfig    = common.MultiChoiceConfig
)

func NewMultiChoiceDialog(config MultiChoiceConfig) Dialog {
	return common.NewMultiChoiceDialog(config)
}
