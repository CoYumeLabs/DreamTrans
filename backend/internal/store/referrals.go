package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/dreamtrans/backend/internal/auth"
	"github.com/dreamtrans/backend/internal/models"
	"github.com/lib/pq"
)

// Referral codes avoid look-alike characters so they survive being read
// aloud or retyped from a poster.
const referralAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

var referralCodePattern = regexp.MustCompile(`^[A-Z0-9]{6,16}$`)

func normalizeReferralCode(code string) string {
	code = strings.ToUpper(strings.TrimSpace(code))
	if !referralCodePattern.MatchString(code) {
		return ""
	}
	return code
}

func newReferralCode() (string, error) {
	data := make([]byte, 8)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	out := make([]byte, len(data))
	for i, b := range data {
		out[i] = referralAlphabet[int(b)%len(referralAlphabet)]
	}
	return string(out), nil
}

// ReferralSummary is what a user sees about their own link.
type ReferralSummary struct {
	Code       string `json:"code"`
	Visits     int    `json:"visits"`
	Registered int    `json:"registered"`
	Verified   int    `json:"verified"`
}

// EnsureReferralCode mints the user's code on first use and returns the
// attribution counters for their link.
func (s *PostgresStore) EnsureReferralCode(ctx context.Context, userID string) (*ReferralSummary, error) {
	var code sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT referral_code FROM users WHERE id=$1 AND is_active`, userID).Scan(&code); err != nil {
		return nil, err
	}
	for attempt := 0; !code.Valid && attempt < 5; attempt++ {
		candidate, err := newReferralCode()
		if err != nil {
			return nil, err
		}
		_, err = s.db.ExecContext(ctx, `UPDATE users SET referral_code=$2 WHERE id=$1 AND referral_code IS NULL`, userID, candidate)
		var pgErr *pq.Error
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			continue
		}
		if err != nil {
			return nil, err
		}
		// A concurrent request may have won; read back whichever code stuck.
		if err := s.db.QueryRowContext(ctx, `SELECT referral_code FROM users WHERE id=$1`, userID).Scan(&code); err != nil {
			return nil, err
		}
	}
	if !code.Valid {
		return nil, errors.New("could not allocate a referral code")
	}
	summary := &ReferralSummary{Code: code.String}
	err := s.db.QueryRowContext(ctx, `SELECT
        (SELECT COUNT(*) FROM invite_visits v WHERE v.referrer_user_id=$1),
        (SELECT COUNT(*) FROM referrals r WHERE r.referrer_user_id=$1),
        (SELECT COUNT(*) FROM referrals r JOIN users u ON u.id=r.referred_user_id WHERE r.referrer_user_id=$1 AND u.email_verified)`,
		userID).Scan(&summary.Visits, &summary.Registered, &summary.Verified)
	return summary, err
}

// ReferrerPreview is the public view of a referral link: only the name the
// referrer chose to show.
type ReferrerPreview struct {
	Name string `json:"referrer_name"`
}

func (s *PostgresStore) PreviewReferral(ctx context.Context, code string) (*ReferrerPreview, error) {
	code = normalizeReferralCode(code)
	if code == "" {
		return nil, sql.ErrNoRows
	}
	p := &ReferrerPreview{}
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(name,'') FROM users WHERE referral_code=$1 AND is_active`, code).Scan(&p.Name)
	return p, err
}

// referrerIDTx resolves a referral code inside the sign-up transaction. An
// unknown code is not an error: attribution must never block a sign-up.
func referrerIDTx(ctx context.Context, tx *sql.Tx, code string) (string, error) {
	code = normalizeReferralCode(code)
	if code == "" {
		return "", nil
	}
	var id string
	err := tx.QueryRowContext(ctx, `SELECT id FROM users WHERE referral_code=$1 AND is_active`, code).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

func recordReferralTx(ctx context.Context, tx *sql.Tx, referrerID string, user *models.User) error {
	if referrerID == "" || referrerID == user.ID {
		return nil
	}
	hash := sha256.Sum256([]byte(auth.CanonicalEmail(user.Email)))
	// A mailbox that already came through a referral keeps its first referrer.
	_, err := tx.ExecContext(ctx, `INSERT INTO referrals(referrer_user_id,referred_user_id,canonical_email_hash) VALUES ($1,$2,$3) ON CONFLICT (canonical_email_hash) DO NOTHING`,
		referrerID, user.ID, hex.EncodeToString(hash[:]))
	return err
}

// ReferrerRow is one entry of the administrator's referral leaderboard.
type ReferrerRow struct {
	UserID     string    `json:"user_id"`
	Email      string    `json:"email"`
	Name       string    `json:"name"`
	Code       string    `json:"code"`
	Visits     int       `json:"visits"`
	Registered int       `json:"registered"`
	Verified   int       `json:"verified"`
	LastAt     time.Time `json:"last_registered_at"`
}

// ListReferrers pages users who brought in at least one sign-up.
func (s *PostgresStore) ListReferrers(ctx context.Context, limit, offset int, search string) ([]ReferrerRow, int, error) {
	if search != "" {
		search = "%" + search + "%"
	}
	filter := ` WHERE ($1='' OR u.email ILIKE $1 OR u.name ILIKE $1 OR u.referral_code ILIKE $1)`
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users u`+filter+` AND EXISTS (SELECT 1 FROM referrals r WHERE r.referrer_user_id=u.id)`, search).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT u.id,u.email,COALESCE(u.name,''),COALESCE(u.referral_code,''),
        (SELECT COUNT(*) FROM invite_visits v WHERE v.referrer_user_id=u.id),
        (SELECT COUNT(*) FROM referrals r WHERE r.referrer_user_id=u.id),
        (SELECT COUNT(*) FROM referrals r JOIN users x ON x.id=r.referred_user_id WHERE r.referrer_user_id=u.id AND x.email_verified),
        (SELECT MAX(r.registered_at) FROM referrals r WHERE r.referrer_user_id=u.id)
        FROM users u`+filter+` AND EXISTS (SELECT 1 FROM referrals r WHERE r.referrer_user_id=u.id)
        ORDER BY 6 DESC,8 DESC,u.id LIMIT $2 OFFSET $3`, search, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()
	items := make([]ReferrerRow, 0)
	for rows.Next() {
		var row ReferrerRow
		if err := rows.Scan(&row.UserID, &row.Email, &row.Name, &row.Code, &row.Visits, &row.Registered, &row.Verified, &row.LastAt); err != nil {
			return nil, 0, err
		}
		items = append(items, row)
	}
	return items, total, rows.Err()
}
