package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
)

// SaveAtomic writes cfg to path as indented JSON, after clearing the resolved
// proxy list (see PasswordForSave). It does not normalize cfg; callers pass a
// config that Normalize already accepted. An existing file is kept as
// path+".bak".
//
// The data is written and synced to a temporary file created beside the
// target, so the final rename stays on the same filesystem; the temporary
// file takes over the existing file's permission bits. Outside Windows the
// backup is a copy and the rename replaces path atomically. On Windows a
// complete backup copy is installed before the original is staged and the
// replacement is renamed into place. If the replacement fails, the original
// is restored and both replacement and rollback errors are returned.
func SaveAtomic(path string, cfg Config) error {
	cfg.PasswordForSave()
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary config: %w", err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if _, err = temp.Write(data); err == nil {
		err = temp.Sync()
	}
	closeErr := temp.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("write temporary config: %w", err)
	}
	if info, statErr := os.Stat(path); statErr == nil {
		if err := os.Chmod(tempPath, info.Mode().Perm()); err != nil {
			return fmt.Errorf("set temporary config permissions: %w", err)
		}
	} else if !os.IsNotExist(statErr) {
		return fmt.Errorf("stat config: %w", statErr)
	}

	backup := path + ".bak"
	if runtime.GOOS == "windows" {
		if _, statErr := os.Stat(path); statErr == nil {
			stage, err := os.CreateTemp(dir, ".config-backup-*.tmp")
			if err != nil {
				return fmt.Errorf("create temporary config backup: %w", err)
			}
			stagePath := stage.Name()
			defer os.Remove(stagePath)
			if err := stage.Close(); err != nil {
				return fmt.Errorf("create temporary config backup: %w", err)
			}
			if err := copyFile(path, stagePath); err != nil {
				return fmt.Errorf("write temporary config backup: %w", err)
			}

			oldBackupPath := ""
			if _, backupErr := os.Stat(backup); backupErr == nil {
				oldBackup, err := os.CreateTemp(dir, ".config-backup-old-*.tmp")
				if err != nil {
					return fmt.Errorf("preserve config backup: %w", err)
				}
				oldBackupPath = oldBackup.Name()
				if err := oldBackup.Close(); err != nil {
					_ = os.Remove(oldBackupPath)
					return fmt.Errorf("preserve config backup: %w", err)
				}
				if err := os.Remove(oldBackupPath); err != nil {
					return fmt.Errorf("preserve config backup: %w", err)
				}
				if err := os.Rename(backup, oldBackupPath); err != nil {
					return fmt.Errorf("preserve config backup: %w", err)
				}
			} else if !os.IsNotExist(backupErr) {
				return fmt.Errorf("stat config backup: %w", backupErr)
			}

			restoreOldBackup := func() error {
				if oldBackupPath == "" {
					return nil
				}
				return restorePreviousConfigBackup(oldBackupPath, backup)
			}
			if err := os.Rename(stagePath, backup); err != nil {
				return errors.Join(fmt.Errorf("install config backup: %w", err), restoreOldBackup())
			}

			oldConfig, err := os.CreateTemp(dir, ".config-old-*.tmp")
			if err != nil {
				return errors.Join(fmt.Errorf("stage current config: %w", err), restoreOldBackup())
			}
			oldConfigPath := oldConfig.Name()
			if err := oldConfig.Close(); err != nil {
				_ = os.Remove(oldConfigPath)
				return errors.Join(fmt.Errorf("stage current config: %w", err), restoreOldBackup())
			}
			if err := os.Remove(oldConfigPath); err != nil {
				return errors.Join(fmt.Errorf("stage current config: %w", err), restoreOldBackup())
			}
			if err := os.Rename(path, oldConfigPath); err != nil {
				return errors.Join(fmt.Errorf("stage current config: %w", err), restoreOldBackup())
			}
			if err := os.Rename(tempPath, path); err != nil {
				rollbackErr := os.Rename(oldConfigPath, path)
				var backupErr error
				if rollbackErr == nil {
					backupErr = restoreOldBackup()
				}
				return errors.Join(fmt.Errorf("replace config: %w", err), rollbackErr, backupErr)
			}
			_ = os.Remove(oldConfigPath)
			if oldBackupPath != "" {
				_ = os.Remove(oldBackupPath)
			}
			return nil
		} else if os.IsNotExist(statErr) {
			if err := os.Rename(tempPath, path); err != nil {
				return fmt.Errorf("replace config: %w", err)
			}
			return nil
		} else {
			return fmt.Errorf("stat config: %w", statErr)
		}
	}
	if _, statErr := os.Stat(path); statErr == nil {
		if err := copyFile(path, backup); err != nil {
			return fmt.Errorf("backup config: %w", err)
		}
	} else if !os.IsNotExist(statErr) {
		return fmt.Errorf("stat config: %w", statErr)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("replace config: %w", err)
	}
	return nil
}

func restorePreviousConfigBackup(source, target string) error {
	if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove current config backup: %w", err)
	}
	return restoreConfigBackup(source, target)
}

func restoreConfigBackup(source, target string) error {
	if err := os.Rename(source, target); err != nil {
		return fmt.Errorf("restore previous config backup: %w", err)
	}
	return nil
}

// copyFile copies source to target, truncating target, and preserves source's
// permission bits. The copy is synced before target is closed.
func copyFile(source, target string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	mode := os.FileMode(0600)
	if info, statErr := in.Stat(); statErr == nil {
		mode = info.Mode().Perm()
	}
	out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_ = out.Chmod(mode)
	_, copyErr := io.Copy(out, in)
	syncErr := out.Sync()
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}
