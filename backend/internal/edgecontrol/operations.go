package edgecontrol

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/dreamtrans/backend/internal/edgeprotocol"
)

func (s *Service) DesiredImage(ctx context.Context, actor, node, image string) error {
	if !immutableImage.MatchString(image) {
		return errors.New("immutable image digest required")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `UPDATE edge_nodes SET desired_image=$2 WHERE id=$1 AND mode<>'revoked'`, node, image)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return sql.ErrNoRows
	}
	details, _ := json.Marshal(map[string]string{"image": image})
	if err := audit(ctx, tx, node, actor, "release_requested", string(details)); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Service) Rotate(ctx context.Context, actor, node string) (string, error) {
	secret, err := edgeprotocol.Secret()
	if err != nil {
		return "", err
	}
	token := node + "." + secret
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `UPDATE edge_nodes SET identity_hash=NULL,mode='disabled',registration_hash=$2,registration_until=now()+interval '15 minutes' WHERE id=$1`, node, edgeprotocol.Hash(token))
	if err != nil {
		return "", err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return "", err
	}
	if count != 1 {
		return "", sql.ErrNoRows
	}
	if err := audit(ctx, tx, node, actor, "identity_rotated", `{}`); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return token, nil
}
func (s *Service) NodeDeployment(ctx context.Context, node string) (map[string]string, error) {
	var image, mode string
	err := s.DB.QueryRowContext(ctx, `SELECT desired_image,mode FROM edge_nodes WHERE id=$1 AND mode<>'revoked'`, node).Scan(&image, &mode)
	if image != "" && !immutableImage.MatchString(image) {
		return nil, errors.New("invalid desired image")
	}
	return map[string]string{"image": image, "mode": mode, "repository": strings.Split(image, "@")[0]}, err
}
