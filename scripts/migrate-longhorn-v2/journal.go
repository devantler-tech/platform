package main

// journal binds a run to the exact worker it observed.
type journal struct {
	Target      string
	NodeUID     string
	ProviderID  string
	MachineUUID string
}
