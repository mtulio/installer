package payload

// KubernetesVersion is the Kubernetes minor version of the release this
// installer belongs to. It is what a payload's declared compatibility range is
// checked against.
//
// It is written here rather than derived at run time because there is nothing
// to derive it from. pkg/version.Raw carries the OpenShift version, and the
// OpenShift-to-Kubernetes mapping is a convention rather than a rule, so
// computing one from the other would be a guess that silently becomes wrong.
// The local control plane's own kube-apiserver would be authoritative, but it
// is not started until long after the payload has to be accepted or rejected.
//
// Keep it in step with the k8s.io/api version in go.mod. TestKubernetesVersion
// reads go.mod and fails if the two disagree, so the bump cannot be forgotten.
const KubernetesVersion = "1.36"
