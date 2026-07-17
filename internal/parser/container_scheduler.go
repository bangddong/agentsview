package parser

// ContainerScheduler exposes the provider-specific identity mapping consumed
// by the engine's shared-container scheduling (see
// SourceCapabilities.ContainerScheduling). It is implemented by the provider
// factory of any agent that declares the capability; the engine resolves it
// once at construction and never computes container identities for providers
// that do not declare it.
type ContainerScheduler interface {
	// SplitContainerMemberPath decomposes a virtual member path into its
	// physical container path and member ID. ok is false for physical
	// container paths and for paths the provider does not own.
	SplitContainerMemberPath(path string) (container, memberID string, ok bool)
	// MemberSessionID returns the persisted full session ID for a member ID.
	MemberSessionID(memberID string) string
	// IsContainerSource reports whether source addresses the whole physical
	// container rather than one virtual member.
	IsContainerSource(source SourceRef) bool
}

// ContainerSchedulerForFactory resolves a factory's ContainerScheduler when
// its declared capabilities request container scheduling.
func ContainerSchedulerForFactory(factory ProviderFactory) (ContainerScheduler, bool) {
	if factory == nil {
		return nil, false
	}
	if factory.Capabilities().Source.ContainerScheduling != CapabilitySupported {
		return nil, false
	}
	scheduler, ok := factory.(ContainerScheduler)
	return scheduler, ok
}
