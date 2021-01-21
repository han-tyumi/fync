package fync

import (
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
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
	return modsDir, dirErr
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
	// Used to log operations.
	*log.Logger

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
	} else if len(serverMods) == 0 {
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

	// define logger
	if o.Logger == nil {
		o.Logger = log.New(ioutil.Discard, "", 0)
	}

	// download each mod to mods directory
	ch := make(chan error, len(serverMods))
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
				err := write(dest, mod, o)
				if err != nil {
					ch <- err
					return
				}
			} else {
				mu.Lock()
				size, exists := localMods[name]
				mu.Unlock()

				if !exists {
					err := write(dest, mod, o)
					if err != nil {
						ch <- err
						return
					}
				} else if size != info.Size() {
					err := move(name, o)
					if err != nil {
						ch <- err
						return
					}

					err = write(dest, mod, o)
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
	}

	if !o.KeepExisting && len(localMods) != 0 {
		os.MkdirAll(backupDir, os.ModeDir|0755)

		ch = make(chan error, len(localMods))
		for mod := range localMods {
			mod := mod
			go func() {
				ch <- move(mod, o)
			}()
		}

		for range localMods {
			err := <-ch
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func move(name string, o *SyncOptions) error {
	from := filepath.Join(modsDir, name)
	to := filepath.Join(backupDir, name)

	o.Printf("moving %q to %q ...\n", from, backupDir)
	if err := os.Rename(from, to); err != nil {
		return err
	}
	return nil
}

func write(dest string, from ServerFile, o *SyncOptions) error {
	o.Printf("writing %q ...\n", dest)

	file, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer file.Close()

	if _, err := from.WriteTo(file); err != nil {
		return err
	}

	return nil
}
