package installer

// Installer defines the interface for installing and uninstalling components.
type Installer interface {
	// Install installs the component.
	Install() error
	// Uninstall removes the component.
	Uninstall() error
}