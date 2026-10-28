package main

// proxyOption leaves legacy private configuration intact unless a deployment
// explicitly supplies its non-secret transport choice.
type proxyOption struct{ value *bool }

// String implements flag.Value without private input values.
func (o *proxyOption) String() string {
	if o.value == nil {
		return ""
	}
	if *o.value {
		return "true"
	}
	return "false"
}

// Set accepts only an explicit, unambiguous transport choice.
func (o *proxyOption) Set(value string) error {
	if value != "true" && value != "false" {
		return errConfiguration
	}
	enabled := value == "true"
	o.value = &enabled
	return nil
}

// IsBoolFlag supports the standard bare enabling flag as well as =false.
func (*proxyOption) IsBoolFlag() bool { return true }
