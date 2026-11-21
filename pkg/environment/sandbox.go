package environment

import "os"

// InSandbox recognizes the Kit sbx@1 marker and legacy Docker sandbox VMs.
func InSandbox() bool {
	return os.Getenv("IS_SANDBOX") == "1" || os.Getenv("SANDBOX_VM_ID") != ""
}
