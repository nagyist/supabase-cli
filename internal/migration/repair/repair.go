package repair

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"github.com/go-errors/errors"
	"github.com/jackc/pgconn"
	"github.com/jackc/pgtype"
	"github.com/jackc/pgx/v4"
	"github.com/spf13/afero"
	"github.com/spf13/viper"
	"github.com/supabase/cli/internal/migration/history"
	"github.com/supabase/cli/internal/migration/list"
	"github.com/supabase/cli/internal/utils"
	"github.com/supabase/cli/internal/utils/parser"
)

const (
	Applied  = "applied"
	Reverted = "reverted"
)

var ErrInvalidVersion = errors.New("invalid version number")

func Run(ctx context.Context, config pgconn.Config, version []string, status string, fsys afero.Fs, options ...func(*pgx.ConnConfig)) error {
	for _, v := range version {
		if _, err := strconv.Atoi(v); err != nil {
			return errors.Errorf("failed to parse %s: %w", v, ErrInvalidVersion)
		}
	}
	repairAll := len(version) == 0
	if repairAll {
		msg := "Do you want to repair the entire migration history table to match local migration files?"
		if shouldRepair := utils.PromptYesNo(msg, false, os.Stdin); !shouldRepair {
			fmt.Fprintln(os.Stderr, "Nothing to repair.")
			return nil
		}
		local, err := list.LoadLocalVersions(fsys)
		if err != nil {
			return err
		}
		version = append(version, local...)
	}
	conn, err := utils.ConnectByConfig(ctx, config, options...)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	// Update migration history
	return UpdateMigrationTable(ctx, conn, version, status, repairAll, fsys)
}

func UpdateMigrationTable(ctx context.Context, conn *pgx.Conn, version []string, status string, repairAll bool, fsys afero.Fs) error {
	schema := &pgconn.Batch{}
	history.AddCreateTableStatements(schema)
	if _, err := conn.PgConn().ExecBatch(ctx, schema).ReadAll(); err != nil {
		return errors.Errorf("failed to create migration table: %w", err)
	}
	// Data statements don't mutate schemas, safe to use statement cache
	batch := &pgx.Batch{}
	if repairAll {
		batch.Queue(history.TRUNCATE_VERSION_TABLE)
	}
	switch status {
	case Applied:
		for _, v := range version {
			f, err := NewMigrationFromVersion(v, fsys)
			if err != nil {
				return err
			}
			batch.Queue(history.INSERT_MIGRATION_VERSION, f.Version, f.Name, f.Lines)
		}
	case Reverted:
		if !repairAll {
			batch.Queue(history.DELETE_MIGRATION_VERSION, version)
		}
	}
	if err := conn.SendBatch(ctx, batch).Close(); err != nil {
		return errors.Errorf("failed to update migration table: %w", err)
	}
	if !repairAll {
		fmt.Fprintf(os.Stderr, "Repaired migration history: %v => %s\n", version, status)
	}
	utils.CmdSuggestion = fmt.Sprintf("Run %s to show the updated migration history.", utils.Aqua("supabase migration list"))
	return nil
}

func CreateMigrationTable(ctx context.Context, conn *pgx.Conn) error {
	batch := pgconn.Batch{}
	history.AddCreateTableStatements(&batch)
	if _, err := conn.PgConn().ExecBatch(ctx, &batch).ReadAll(); err != nil {
		return errors.Errorf("failed to create migration table: %w", err)
	}
	return nil
}

func InsertVersionSQL(batch *pgconn.Batch, version, name string, stats []string) {
	// Create history table if not exists
	history.AddCreateTableStatements(batch)
	encoded := []byte{'{'}
	for i, line := range stats {
		if i > 0 {
			encoded = append(encoded, ',')
		}
		encoded = append(encoded, pgtype.QuoteArrayElementIfNeeded(line)...)
	}
	encoded = append(encoded, '}')
	batch.ExecParams(
		history.INSERT_MIGRATION_VERSION,
		[][]byte{[]byte(version), []byte(name), encoded},
		[]uint32{pgtype.TextOID, pgtype.TextOID, pgtype.TextArrayOID},
		[]int16{pgtype.TextFormatCode, pgtype.TextFormatCode, pgtype.TextFormatCode},
		nil,
	)
}

func GetMigrationFile(version string, fsys afero.Fs) (string, error) {
	path := filepath.Join(utils.MigrationsDir, version+"_*.sql")
	matches, err := afero.Glob(fsys, path)
	if err != nil {
		return "", errors.Errorf("failed to glob migration files: %w", err)
	}
	if len(matches) == 0 {
		return "", errors.Errorf("glob %s: %w", path, os.ErrNotExist)
	}
	return matches[0], nil
}

type MigrationFile struct {
	Lines   []string
	Version string
	Name    string
}

func NewMigrationFromVersion(version string, fsys afero.Fs) (*MigrationFile, error) {
	name, err := GetMigrationFile(version, fsys)
	if err != nil {
		return nil, err
	}
	return NewMigrationFromFile(name, fsys)
}

func NewMigrationFromFile(path string, fsys afero.Fs) (*MigrationFile, error) {
	sql, err := fsys.Open(path)
	if err != nil {
		return nil, errors.Errorf("failed to open migration file: %w", err)
	}
	defer sql.Close()
	// Unless explicitly specified, Use file length as max buffer size
	if !viper.IsSet("SCANNER_BUFFER_SIZE") {
		if fi, err := sql.Stat(); err == nil {
			if size := int(fi.Size()); size > parser.MaxScannerCapacity {
				parser.MaxScannerCapacity = size
			}
		}
	}
	file, err := NewMigrationFromReader(sql)
	if err == nil {
		// Parse version from file name
		filename := filepath.Base(path)
		matches := utils.MigrateFilePattern.FindStringSubmatch(filename)
		if len(matches) > 2 {
			file.Version = matches[1]
			file.Name = matches[2]
		}
	}
	return file, err
}

func NewMigrationFromReader(sql io.Reader) (*MigrationFile, error) {
	lines, err := parser.SplitAndTrim(sql)
	if err != nil {
		return nil, err
	}
	return &MigrationFile{Lines: lines}, nil
}

func (m *MigrationFile) ExecBatch(ctx context.Context, conn *pgx.Conn) error {
	// Batch migration commands, without using statement cache
	batch := &pgconn.Batch{}
	for _, line := range m.Lines {
		batch.ExecParams(line, nil, nil, nil, nil)
	}
	// Insert into migration history
	if len(m.Version) > 0 {
		InsertVersionSQL(batch, m.Version, m.Name, m.Lines)
	}
	// ExecBatch is implicitly transactional
	if result, err := conn.PgConn().ExecBatch(ctx, batch).ReadAll(); err != nil {
		// Defaults to printing the last statement on error
		stat := history.INSERT_MIGRATION_VERSION
		i := len(result)
		if i < len(m.Lines) {
			stat = m.Lines[i]
		}
		return errors.Errorf("%w\nAt statement %d: %s", err, i, stat)
	}
	return nil
}

func (m *MigrationFile) ExecBatchWithCache(ctx context.Context, conn *pgx.Conn) error {
	// Data statements don't mutate schemas, safe to use statement cache
	batch := pgx.Batch{}
	for _, line := range m.Lines {
		batch.Queue(line)
	}
	// No need to track version here because there are no schema changes
	if err := conn.SendBatch(ctx, &batch).Close(); err != nil {
		return errors.Errorf("failed to send batch: %w", err)
	}
	return nil
}
