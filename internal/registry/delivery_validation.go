package registry

import "fmt"

// validateDeliveryTTL is shared by ingress and non-HTTP Dispatch callers. The
// ingress call stays before payload validation to preserve HTTP error precedence.
func validateDeliveryTTL(ttl *int) error {
	if ttl == nil {
		return nil
	}
	if *ttl < 0 {
		return fmt.Errorf("ttl_seconds must not be negative")
	}
	if int64(*ttl) > maxTTLSeconds {
		return fmt.Errorf("ttl_seconds must be <= %d", maxTTLSeconds)
	}
	return nil
}
