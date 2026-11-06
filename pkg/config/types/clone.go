package types

import (
	"encoding/json"
	"fmt"
)

func CloneThroughJSON(oldConfig, newConfig any) {
	if err := TryCloneThroughJSON(oldConfig, newConfig); err != nil {
		panic(err.Error())
	}
}

// TryCloneThroughJSON is CloneThroughJSON for an upgrade whose new version
// can reject a value the old one accepted.
func TryCloneThroughJSON(oldConfig, newConfig any) error {
	o, err := json.Marshal(oldConfig)
	if err != nil {
		return fmt.Errorf("marshalling old: %w", err)
	}

	if err := json.Unmarshal(o, newConfig); err != nil {
		return fmt.Errorf("unmarshalling new: %w", err)
	}
	return nil
}
