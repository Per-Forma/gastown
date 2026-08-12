package dog

import (
	"errors"
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/mail"
)

// ArchivePluginDispatchMail archives direct plugin dispatch messages that have
// already been consumed by a dog. It is safe to call repeatedly: archived
// messages no longer appear in the open mailbox listing.
func ArchivePluginDispatchMail(townRoot, dogName string) (int, error) {
	if err := validateDogName(dogName); err != nil {
		return 0, err
	}

	dogAddress := fmt.Sprintf("deacon/dogs/%s", dogName)
	router := mail.NewRouterWithTownRoot(townRoot, townRoot)
	mailbox, err := router.GetMailbox(dogAddress)
	if err != nil {
		return 0, err
	}

	messages, err := mailbox.List()
	if err != nil {
		return 0, err
	}

	var archiveErrors []error
	closed := 0
	for _, msg := range messages {
		if !strings.HasPrefix(msg.Subject, "Plugin: ") {
			continue
		}
		if mail.AddressToIdentity(msg.To) != mail.AddressToIdentity(dogAddress) {
			continue
		}
		sender := mail.AddressToIdentity(msg.From)
		if sender != "deacon/" && sender != "daemon" {
			continue
		}
		if err := mailbox.Archive(msg.ID); err != nil {
			archiveErrors = append(archiveErrors, fmt.Errorf("archive %s: %w", msg.ID, err))
			continue
		}
		closed++
	}

	return closed, errors.Join(archiveErrors...)
}
