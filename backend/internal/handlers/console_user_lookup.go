package handlers

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
)

var errConsoleUserNotFound = errors.New("user not found")

// resolveConsoleUser accepts either a user UUID or an email address from a
// console form and returns the user ID. Operators know emails, not UUIDs.
func (h *AdminHandler) resolveConsoleUser(ctx context.Context, raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", errConsoleUserNotFound
	}
	if _, err := uuid.Parse(value); err == nil {
		return value, nil
	}
	if !strings.Contains(value, "@") {
		return "", errConsoleUserNotFound
	}
	user, err := h.store.GetUserByEmail(ctx, strings.ToLower(value))
	if err != nil {
		return "", err
	}
	if user == nil {
		return "", errConsoleUserNotFound
	}
	return user.ID, nil
}
