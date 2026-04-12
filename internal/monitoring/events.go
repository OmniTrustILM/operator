package monitoring

// Event reason constants for Kubernetes events emitted by the ILM operator.
const (
	ReasonDeployed           = "Deployed"
	ReasonUpdated            = "Updated"
	ReasonDeleting           = "Deleting"
	ReasonDegraded           = "Degraded"
	ReasonRecovered          = "Recovered"
	ReasonRegistered         = "Registered"
	ReasonRegistrationFailed = "RegistrationFailed"
	ReasonConfigChanged      = "ConfigChanged"
	ReasonMissingSecret      = "MissingSecret"
	ReasonMissingConfigMap   = "MissingConfigMap"
)
