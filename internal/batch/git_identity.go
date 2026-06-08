package batch

// gitIdentity is the resolved git author identity (user.name + user.email).
// It is the value type returned by gitIdentityResolver.resolve and is
// referenced only by the resolver and its tests.
type gitIdentity struct {
	Name  string
	Email string
}
