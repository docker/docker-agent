package latest

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// StreamIdleTimeout is a positive duration or -1 to disable the idle timer.
type StreamIdleTimeout struct { //nolint:recvcheck // Value marshaling supports non-pointer values; unmarshaling requires a pointer.
	time.Duration
}

const DisabledStreamIdleTimeout time.Duration = -1

func (d *StreamIdleTimeout) Validate() error {
	if d != nil && d.Duration != DisabledStreamIdleTimeout && d.Duration <= 0 {
		return errors.New("must be a positive duration or integer -1 to disable")
	}
	return nil
}

func (d *StreamIdleTimeout) UnmarshalYAML(unmarshal func(any) error) error {
	var n int64
	if err := unmarshal(&n); err == nil {
		if n != -1 {
			return errors.New("must be a positive duration or integer -1 to disable")
		}
		d.Duration = DisabledStreamIdleTimeout
		return nil
	}
	var s string
	if err := unmarshal(&s); err != nil {
		return err
	}
	return d.parse(s)
}

func (d *StreamIdleTimeout) parse(s string) error {
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	if v <= 0 {
		return errors.New("must be a positive duration or integer -1 to disable")
	}
	d.Duration = v
	return nil
}

func (d StreamIdleTimeout) MarshalYAML() (any, error) {
	if err := d.Validate(); err != nil {
		return nil, err
	}
	if d.Duration == DisabledStreamIdleTimeout {
		return -1, nil
	}
	return d.String(), nil
}

func (d *StreamIdleTimeout) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		return d.parse(s)
	}
	var n int64
	if err := json.Unmarshal(data, &n); err != nil || n != -1 {
		return errors.New("must be a positive duration or integer -1 to disable")
	}
	d.Duration = DisabledStreamIdleTimeout
	return nil
}

func (d StreamIdleTimeout) MarshalJSON() ([]byte, error) {
	v, err := d.MarshalYAML()
	if err != nil {
		return nil, err
	}
	return json.Marshal(v)
}
