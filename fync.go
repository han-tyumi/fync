package fync

import (
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

var installDir, modsDir, backupDir string
var dirErr error

func init() {
	switch runtime.GOOS {
	case "windows":
		fallthrough
	case "darwin":
		installDir, dirErr = os.UserConfigDir()
	case "linux":
		installDir, dirErr = os.UserHomeDir()
	default:
		dirErr = fmt.Errorf("%q is unsupported", runtime.GOOS)
	}

	if dirErr != nil {
		return
	}

	if runtime.GOOS == "darwin" {
		installDir = filepath.Join(installDir, "minecraft")
	} else {
		installDir = filepath.Join(installDir, ".minecraft")
	}

	modsDir = filepath.Join(installDir, "mods")
	backupDir = filepath.Join(modsDir, "backup")
}

// InstallDir returns the Minecraft installation directory.
func InstallDir() (string, error) {
	return installDir, dirErr
}

// ModsDir returns the Minecraft mods directory.
func ModsDir() (string, error) {
	return modsDir, dirErr
}

// BackupDir returns the backup directory for Minecraft mods that were not on the server.
func BackupDir() (string, error) {
	return backupDir, dirErr
}

// ServerFile represents a server mod file that can be written to another file
// and is able to provide its FileInfo and be closed.
type ServerFile interface {
	io.WriterTo
	io.Closer
	Stat() (os.FileInfo, error)
}

// Server represents a MinecraftForge server.
type Server interface {
	// Mods returns a slice of mod ServerFiles the server is using.
	Mods() ([]ServerFile, error)
}

// SyncOptions contains options for the Sync function.
type SyncOptions struct {
	// Called when a mod is being written.
	OnWrite func(from os.FileInfo, to string)

	// Called when an existing mod is being backed up.
	OnBackup func(name, from, to string)

	// Called when a task's progress has updated.
	OnProgress func(task string, curr, total int)

	// Whether or not to keep existing mods by not backing up them up if they're not on the server.
	KeepExisting bool

	// Whether to overwite existing local mods with same name as a server mod.
	Force bool
}

// Sync will sync the server's mods with the user's local Minecraft mods.
func Sync(s Server, o *SyncOptions) error {
	if dirErr != nil {
		return dirErr
	}

	// obtain list of mods
	serverMods, err := s.Mods()
	if err != nil {
		return err
	}

	total := len(serverMods)
	if total == 0 {
		return errors.New("no server mods to sync")
	}

	// make sure mods directory exists
	if err := os.MkdirAll(modsDir, os.ModeDir|0755); err != nil {
		return err
	}

	// determine local mods
	var localMods map[string]int64
	if !(o.KeepExisting && o.Force) {
		files, err := ioutil.ReadDir(modsDir)
		if err != nil {
			return err
		}

		localMods = make(map[string]int64)
		for i := range files {
			if !files[i].IsDir() && strings.HasSuffix(files[i].Name(), ".jar") {
				localMods[files[i].Name()] = files[i].Size()
			}
		}
	}

	curr := 0
	if o.OnProgress != nil {
		o.OnProgress("write", curr, total)
	}

	// download each mod to mods directory
	ch := make(chan error, total)
	var mu sync.Mutex
	for i := range serverMods {
		go func(mod ServerFile) {
			defer mod.Close()

			info, err := mod.Stat()
			if err != nil {
				ch <- err
				return
			}

			name := info.Name()
			dest := filepath.Join(modsDir, name)

			// write server mod to local mods dir
			if o.Force {
				err := write(mod, dest, o)
				if err != nil {
					ch <- err
					return
				}
			} else {
				mu.Lock()
				size, exists := localMods[name]
				mu.Unlock()

				if !exists {
					err := write(mod, dest, o)
					if err != nil {
						ch <- err
						return
					}
				} else if size != info.Size() {
					err := backup(name, o)
					if err != nil {
						ch <- err
						return
					}

					err = write(mod, dest, o)
					if err != nil {
						ch <- err
						return
					}
				}
			}

			if !o.KeepExisting {
				mu.Lock()
				delete(localMods, name)
				mu.Unlock()
			}

			ch <- nil
		}(serverMods[i])
	}

	// TODO: refactor this error channel pattern into type
	for range serverMods {
		err := <-ch
		if err != nil {
			close(ch)
			return err
		}

		if o.OnProgress != nil {
			curr++
			o.OnProgress("write", curr, total)
		}
	}

	total = len(localMods)
	if !o.KeepExisting && total != 0 {
		os.MkdirAll(backupDir, os.ModeDir|0755)

		if o.OnProgress != nil {
			curr = 0
			o.OnProgress("backup", curr, total)
		}

		ch = make(chan error, total)
		for mod := range localMods {
			mod := mod
			go func() {
				ch <- backup(mod, o)
			}()
		}

		for range localMods {
			err := <-ch
			if err != nil {
				close(ch)
				return err
			}

			if o.OnProgress != nil {
				curr++
				o.OnProgress("backup", curr, total)
			}
		}
	}

	return nil
}

func backup(name string, o *SyncOptions) error {
	from := filepath.Join(modsDir, name)
	to := filepath.Join(backupDir, name)

	if o.OnBackup != nil {
		o.OnBackup(name, from, to)
	}

	if err := os.Rename(from, to); err != nil {
		return err
	}
	return nil
}

func write(from ServerFile, to string, o *SyncOptions) error {
	if o.OnWrite != nil {
		info, err := from.Stat()
		if err != nil {
			return err
		}
		o.OnWrite(info, to)
	}

	file, err := os.Create(to)
	if err != nil {
		return err
	}
	defer file.Close()

	if _, err := from.WriteTo(file); err != nil {
		return err
	}

	return nil
}
