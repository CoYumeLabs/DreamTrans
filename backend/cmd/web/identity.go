package main

import (
	"context"
	"fmt"
	"log"
	"net/mail"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/dreamtrans/backend/internal/auth"
	"github.com/dreamtrans/backend/internal/models"
)

func newBootstrapAdministrator(
	tenantID string,
	email string,
	passwordHash string,
) *models.User {
	return &models.User{
		TenantID:      tenantID,
		Email:         email,
		PasswordHash:  passwordHash,
		Name:          "Administrator",
		Role:          "super_admin",
		IsActive:      true,
		EmailVerified: true,
	}
}

func (app *Application) bootstrapAdmin(ctx context.Context) error {
	email := strings.ToLower(strings.TrimSpace(os.Getenv("ADMIN_EMAIL")))
	password := os.Getenv("ADMIN_PASSWORD")
	if email == "" && password == "" {
		log.Println("ADMIN_EMAIL/ADMIN_PASSWORD not set; no bootstrap administrator will be created")
		return nil
	}
	if email == "" || password == "" {
		return fmt.Errorf("ADMIN_EMAIL and ADMIN_PASSWORD must be configured together")
	}
	address, emailErr := mail.ParseAddress(email)
	if emailErr != nil || !strings.EqualFold(address.Address, email) {
		return fmt.Errorf("ADMIN_EMAIL is invalid")
	}
	if utf8.RuneCountInString(password) < 16 || len(password) > 72 {
		return fmt.Errorf("ADMIN_PASSWORD must be 16-72 characters and at most 72 bytes")
	}
	existing, err := app.Store.GetUserByEmail(ctx, email)
	if err != nil {
		return err
	}
	if existing != nil {
		if existing.Role != "super_admin" {
			return fmt.Errorf("bootstrap email %q already belongs to a non-super-admin user", email)
		}
		if !existing.IsActive {
			passwordHash, hashErr := auth.HashPassword(password)
			if hashErr != nil {
				return hashErr
			}
			reactivated, reactivateErr := app.Store.ReactivateDisabledLegacyAdmin(ctx, existing.ID, passwordHash)
			if reactivateErr != nil {
				return reactivateErr
			}
			if !reactivated {
				return fmt.Errorf("bootstrap super administrator %q is disabled and cannot be reset automatically", email)
			}
			log.Printf("Reactivated migrated legacy administrator %s with the explicitly configured password", strconv.Quote(email))
		}
		return nil
	}
	tenant, err := app.Store.GetDefaultTenant(ctx)
	if err != nil {
		return fmt.Errorf("default tenant unavailable: %w", err)
	}
	if tenant == nil {
		return fmt.Errorf("default tenant unavailable")
	}
	passwordHash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	user := newBootstrapAdministrator(tenant.ID, email, passwordHash)
	if err := app.Store.CreateUser(ctx, user); err != nil {
		return err
	}
	if app.Billing != nil {
		if err := app.Billing.GrantTrialCredit(ctx, user.ID); err != nil {
			return fmt.Errorf("grant bootstrap administrator credit: %w", err)
		}
	}
	log.Printf("Created bootstrap super administrator %s", strconv.Quote(email))
	return nil
}

func (app *Application) validateCurrentClaims(ctx context.Context, claims *auth.UserClaims) error {
	if app.Store == nil {
		return nil
	}
	if claims == nil {
		return fmt.Errorf("missing claims")
	}
	user, err := app.Store.GetUserByID(ctx, claims.UserID)
	if err != nil {
		return err
	}
	if user == nil || !user.IsActive || user.TenantID != claims.TenantID {
		return fmt.Errorf("account is inactive")
	}
	// Role and email changes take effect immediately for this request.
	claims.Role = user.Role
	claims.Email = user.Email
	return nil
}

// registrationHourlyLimit reads REGISTRATION_RATE_LIMIT_PER_HOUR (default 5):
// how many sign-ups or verification resends one address may attempt per hour.
func registrationHourlyLimit() int {
	if value, err := strconv.Atoi(strings.TrimSpace(os.Getenv("REGISTRATION_RATE_LIMIT_PER_HOUR"))); err == nil && value > 0 {
		return value
	}
	return 5
}
