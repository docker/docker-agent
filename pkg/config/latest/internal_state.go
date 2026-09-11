package latest

// InternalModelOverrideState returns opaque loader-owned state. It is not part
// of the configuration schema and must not affect serialization.
func (t *Config) InternalModelOverrideState() any {
	if t == nil {
		return nil
	}
	return t.modelOverrideState
}

// SetInternalModelOverrideState stores opaque loader-owned state outside the
// public configuration schema.
func (t *Config) SetInternalModelOverrideState(state any) {
	if t != nil {
		t.modelOverrideState = state
	}
}
