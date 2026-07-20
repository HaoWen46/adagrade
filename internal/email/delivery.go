package email

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/HaoWen46/adagrade/internal/domain"
)

const deliveryCorrelationHeader = "X-ADA-Marker-Delivery-Key"

// messageCorrelationID returns the local part used for a Message-ID and provider
// correlation. Stable delivery keys are hashed so filenames and wire headers
// cannot expose caller data or contain unsafe path/header characters. Empty keys
// retain the legacy random-per-attempt behavior.
func messageCorrelationID(deliveryKey string) (string, error) {
	if deliveryKey == "" {
		return randomID()
	}
	sum := sha256.Sum256([]byte(deliveryKey))
	// 128 opaque bits are ample for correlation/idempotency while keeping the
	// longest MIME boundary ("adamarker-mixed-" + id) below RFC 2046's 70-char
	// boundary limit.
	return "delivery-" + hex.EncodeToString(sum[:16]), nil
}

func rfcMessageID(correlationID string) string {
	return fmt.Sprintf("<%s@adamarker.local>", correlationID)
}

func definitelyNotAccepted(err error) error {
	return domain.NewEmailDeliveryError(domain.EmailDeliveryDefinitelyNotAccepted, err)
}

func outcomeUnknown(err error) error {
	return domain.NewEmailDeliveryError(domain.EmailDeliveryOutcomeUnknown, err)
}
