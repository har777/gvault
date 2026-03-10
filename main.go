package main

import (
	"bufio"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"time"
)

const (
	defaultBranch   = "main"
	encryptedSuffix = ".enc"
)

type Config struct {
	Password string   `json:"password"`
	GitURL   string   `json:"git_url"`
	Folders  []string `json:"folders"`
}

type Paths struct {
	Root    string
	Config  string
	Log     string
	Mirror  string
	Backups string
}

type FileChange struct {
	Status string
	Path   string
}

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, in io.Reader, out io.Writer) error {
	paths, err := defaultPaths()
	if err != nil {
		return err
	}

	if len(args) == 0 {
		return fmt.Errorf("usage: gvault <init|backup|fetch>")
	}

	switch args[0] {
	case "init":
		return runInit(paths, in, out)
	case "backup":
		return runBackup(paths, out)
	case "fetch":
		return runFetch(paths, args[1:], out)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func defaultPaths() (Paths, error) {
	usr, err := user.Current()
	if err != nil {
		return Paths{}, fmt.Errorf("failed to resolve home directory: %w", err)
	}

	root := filepath.Join(usr.HomeDir, ".gvault")
	return Paths{
		Root:    root,
		Config:  filepath.Join(root, "config.json"),
		Log:     filepath.Join(root, "logs.txt"),
		Mirror:  filepath.Join(root, "mirror"),
		Backups: filepath.Join(root, "backups"),
	}, nil
}

func runInit(paths Paths, in io.Reader, out io.Writer) error {
	if _, err := os.Stat(paths.Config); err == nil {
		return fmt.Errorf("config already exists at %s", paths.Config)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("failed to check config: %w", err)
	}

	if err := os.MkdirAll(paths.Root, 0o700); err != nil {
		return fmt.Errorf("failed to create gvault directory: %w", err)
	}

	cfg, err := promptConfig(in, out)
	if err != nil {
		return err
	}
	if err := validateConfig(cfg); err != nil {
		return err
	}
	if err := writeConfig(paths.Config, cfg); err != nil {
		return err
	}
	if err := appendLog(paths.Log, "initialized config"); err != nil {
		return err
	}

	_, err = fmt.Fprintf(out, "created %s\n", paths.Config)
	return err
}

func runBackup(paths Paths, out io.Writer) error {
	cfg, err := loadConfig(paths.Config)
	if err != nil {
		return err
	}
	if err := validateConfig(cfg); err != nil {
		return err
	}

	if err := ensureLocalRepo(paths.Mirror); err != nil {
		return err
	}
	if err := ensureRemoteRepo(paths.Backups, cfg.GitURL); err != nil {
		return err
	}
	if err := updateRemoteRepo(paths.Backups); err != nil {
		return fmt.Errorf("failed to update backups repo: %w", err)
	}
	if err := syncMirror(paths.Mirror, cfg.Folders); err != nil {
		return err
	}
	if err := runCommand("", "git", "-C", paths.Mirror, "add", "-A", "."); err != nil {
		return fmt.Errorf("failed to stage mirror changes: %w", err)
	}

	changes, err := stagedChanges(paths.Mirror)
	if err != nil {
		return err
	}
	if len(changes) == 0 {
		if err := appendLog(paths.Log, "backup skipped: no changes"); err != nil {
			return err
		}
		_, err := fmt.Fprintln(out, "backup skipped: no changes")
		return err
	}

	updatedCount, err := applyChangesToBackup(paths.Mirror, paths.Backups, cfg.Password, changes)
	if err != nil {
		return err
	}
	if err := pruneEmptyDirs(paths.Backups); err != nil {
		return err
	}
	if err := runCommand("", "git", "-C", paths.Backups, "add", "-A", "."); err != nil {
		return fmt.Errorf("failed to stage backup changes: %w", err)
	}

	if err := commitRepo(paths.Backups, "gvault backup "+time.Now().Format(time.RFC3339)); err != nil {
		return err
	}
	if err := runCommand("", "git", "-C", paths.Backups, "push", "-u", "origin", defaultBranch); err != nil {
		return fmt.Errorf("failed to push backup: %w", err)
	}
	if err := commitRepo(paths.Mirror, "gvault mirror "+time.Now().Format(time.RFC3339)); err != nil {
		return err
	}
	if err := appendLog(paths.Log, "backup completed"); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "backup completed: %d file(s) updated\n", updatedCount)
	return err
}

func runFetch(paths Paths, args []string, out io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: gvault fetch <destination_path> [git_hash]")
	}

	cfg, err := loadConfig(paths.Config)
	if err != nil {
		return err
	}
	if err := validateConfig(cfg); err != nil {
		return err
	}

	if err := ensureRemoteRepo(paths.Backups, cfg.GitURL); err != nil {
		return err
	}
	if err := updateRemoteRepo(paths.Backups); err != nil {
		return fmt.Errorf("failed to fetch backups repo: %w", err)
	}

	destination, err := filepath.Abs(args[0])
	if err != nil {
		return fmt.Errorf("failed to resolve destination path: %w", err)
	}
	ref := "HEAD"
	if len(args) > 1 {
		ref = args[1]
	}

	files, err := listEncryptedFilesFromCommit(paths.Backups, ref)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("no encrypted files found in commit %s", ref)
	}

	if err := os.MkdirAll(destination, 0o755); err != nil {
		return fmt.Errorf("failed to create destination path: %w", err)
	}
	tempDir, err := os.MkdirTemp(paths.Root, "fetch-*")
	if err != nil {
		return fmt.Errorf("failed to create temporary fetch directory: %w", err)
	}
	defer os.RemoveAll(tempDir)

	for _, filePath := range files {
		tempPath := filepath.Join(tempDir, filePath)
		if err := os.MkdirAll(filepath.Dir(tempPath), 0o700); err != nil {
			return fmt.Errorf("failed to create temporary restore directory: %w", err)
		}
		if err := writeFileFromCommit(paths.Backups, ref, filePath, tempPath); err != nil {
			return err
		}
		plaintext, err := decryptFile(cfg.Password, tempPath)
		if err != nil {
			return err
		}
		restorePath := filepath.Join(destination, strings.TrimSuffix(filePath, encryptedSuffix))
		if err := os.MkdirAll(filepath.Dir(restorePath), 0o755); err != nil {
			return fmt.Errorf("failed to create restore directory: %w", err)
		}
		if err := os.WriteFile(restorePath, plaintext, 0o600); err != nil {
			return fmt.Errorf("failed to write restored file %s: %w", restorePath, err)
		}
	}

	if err := appendLog(paths.Log, "fetch completed to "+destination); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "restored backup to %s\n", destination)
	return err
}

func promptConfig(in io.Reader, out io.Writer) (Config, error) {
	reader := bufio.NewReader(in)

	password, err := prompt(reader, out, "Password: ")
	if err != nil {
		return Config{}, fmt.Errorf("failed to read password: %w", err)
	}
	gitURL, err := prompt(reader, out, "Git URL: ")
	if err != nil {
		return Config{}, fmt.Errorf("failed to read git url: %w", err)
	}
	foldersText, err := prompt(reader, out, "Folders (comma-separated absolute paths): ")
	if err != nil {
		return Config{}, fmt.Errorf("failed to read folders: %w", err)
	}
	folders, err := parseFolders(foldersText)
	if err != nil {
		return Config{}, err
	}

	return Config{Password: password, GitURL: gitURL, Folders: folders}, nil
}

func prompt(reader *bufio.Reader, out io.Writer, label string) (string, error) {
	if _, err := fmt.Fprint(out, label); err != nil {
		return "", err
	}
	text, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSpace(text), nil
}

func parseFolders(input string) ([]string, error) {
	parts := strings.Split(input, ",")
	folders := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if !filepath.IsAbs(part) {
			return nil, fmt.Errorf("folder path must be absolute: %s", part)
		}
		folders = append(folders, filepath.Clean(part))
	}
	if len(folders) == 0 {
		return nil, fmt.Errorf("at least one folder is required")
	}
	return folders, nil
}

func validateConfig(cfg Config) error {
	if cfg.Password == "" {
		return fmt.Errorf("config password is required")
	}
	if cfg.GitURL == "" {
		return fmt.Errorf("config git_url is required")
	}
	if len(cfg.Folders) == 0 {
		return fmt.Errorf("config folders must not be empty")
	}
	for _, folder := range cfg.Folders {
		if !filepath.IsAbs(folder) {
			return fmt.Errorf("config folder path must be absolute: %s", folder)
		}
	}
	return nil
}

func writeConfig(path string, cfg Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to encode config: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("failed to write config: %w", err)
	}
	return nil
}

func loadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("failed to read config: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("failed to decode config: %w", err)
	}
	return cfg, nil
}

func appendLog(path, message string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("failed to create log directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("failed to open log file: %w", err)
	}
	defer f.Close()
	if _, err := fmt.Fprintf(f, "%s %s\n", time.Now().Format(time.RFC3339), message); err != nil {
		return fmt.Errorf("failed to write log entry: %w", err)
	}
	return nil
}

func ensureLocalRepo(repo string) error {
	if _, err := os.Stat(filepath.Join(repo, ".git")); err == nil {
		return ensureMainBranch(repo)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("failed to inspect mirror repo: %w", err)
	}

	if err := os.MkdirAll(repo, 0o700); err != nil {
		return fmt.Errorf("failed to create mirror directory: %w", err)
	}
	if err := runCommand("", "git", "-C", repo, "init"); err != nil {
		return fmt.Errorf("failed to initialize mirror repo: %w", err)
	}
	return ensureMainBranch(repo)
}

func ensureRemoteRepo(repo, gitURL string) error {
	if _, err := os.Stat(filepath.Join(repo, ".git")); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("failed to inspect backups repo: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(repo), 0o700); err != nil {
		return fmt.Errorf("failed to create gvault parent directory: %w", err)
	}
	if err := runCommand("", "git", "clone", gitURL, repo); err != nil {
		return fmt.Errorf("failed to clone backups repo: %w", err)
	}
	return ensureMainBranch(repo)
}

func updateRemoteRepo(repo string) error {
	if err := runCommand("", "git", "-C", repo, "fetch", "origin"); err != nil {
		return err
	}
	if err := ensureMainBranch(repo); err != nil {
		return err
	}
	remoteRef := "refs/remotes/origin/" + defaultBranch
	if err := runCommand("", "git", "-C", repo, "show-ref", "--verify", "--quiet", remoteRef); err != nil {
		return nil
	}
	if err := runCommand("", "git", "-C", repo, "rebase", "origin/"+defaultBranch); err != nil {
		return err
	}
	return pruneEmptyDirs(repo)
}

func ensureMainBranch(repo string) error {
	remoteRef := "refs/remotes/origin/" + defaultBranch
	if err := runCommand("", "git", "-C", repo, "show-ref", "--verify", "--quiet", remoteRef); err == nil {
		if err := runCommand("", "git", "-C", repo, "checkout", "-B", defaultBranch, "--track", "origin/"+defaultBranch); err != nil {
			return fmt.Errorf("failed to check out %s branch: %w", defaultBranch, err)
		}
		return nil
	}
	if err := runCommand("", "git", "-C", repo, "checkout", "-B", defaultBranch); err != nil {
		return fmt.Errorf("failed to create %s branch: %w", defaultBranch, err)
	}
	return nil
}

func syncMirror(mirrorRepo string, folders []string) error {
	rootNames, err := rootNamesForFolders(folders)
	if err != nil {
		return err
	}
	for i, folder := range folders {
		target := filepath.Join(mirrorRepo, rootNames[i])
		if err := os.RemoveAll(target); err != nil {
			return fmt.Errorf("failed to reset mirror directory %s: %w", target, err)
		}
		if err := copyTree(folder, target); err != nil {
			return err
		}
	}
	return nil
}

func rootNamesForFolders(folders []string) ([]string, error) {
	names := make([]string, 0, len(folders))
	for _, folder := range folders {
		name, err := rootName(folder)
		if err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, nil
}

func rootName(folder string) (string, error) {
	if !filepath.IsAbs(folder) {
		return "", fmt.Errorf("folder path must be absolute: %s", folder)
	}
	clean := filepath.ToSlash(filepath.Clean(folder))
	clean = strings.TrimPrefix(clean, "/")
	clean = strings.ReplaceAll(clean, "/", "__")
	if clean == "" {
		return "", fmt.Errorf("folder path must not be root: %s", folder)
	}
	return clean, nil
}

func copyTree(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("failed to stat source folder %s: %w", src, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("configured path is not a folder: %s", src)
	}
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return fmt.Errorf("failed to create mirror destination %s: %w", dst, err)
	}
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("failed to walk %s: %w", path, walkErr)
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return fmt.Errorf("failed to compute relative path for %s: %w", path, err)
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			if err := os.MkdirAll(target, 0o700); err != nil {
				return fmt.Errorf("failed to create mirror directory %s: %w", target, err)
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("failed to read file info for %s: %w", path, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlinks are not supported: %s", path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("failed to read source file %s: %w", path, err)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return fmt.Errorf("failed to create parent directory for %s: %w", target, err)
		}
		if err := os.WriteFile(target, data, info.Mode().Perm()); err != nil {
			return fmt.Errorf("failed to write mirror file %s: %w", target, err)
		}
		return nil
	})
}

func stagedChanges(repo string) ([]FileChange, error) {
	cmd := exec.Command("git", "-C", repo, "diff", "--cached", "--name-status", "--no-renames")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to inspect staged changes: %w", err)
	}
	return parseNameStatus(string(out))
}

func parseNameStatus(output string) ([]FileChange, error) {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	changes := make([]FileChange, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("unexpected git diff output: %s", line)
		}
		changes = append(changes, FileChange{Status: parts[0], Path: parts[1]})
	}
	return changes, nil
}

func applyChangesToBackup(mirrorRepo, backupRepo, password string, changes []FileChange) (int, error) {
	updated := 0
	for _, change := range changes {
		backupPath := filepath.Join(backupRepo, change.Path) + encryptedSuffix
		if change.Status == "D" {
			if err := os.Remove(backupPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				return 0, fmt.Errorf("failed to remove encrypted file %s: %w", backupPath, err)
			}
			continue
		}

		sourcePath := filepath.Join(mirrorRepo, change.Path)
		if err := encryptFile(password, sourcePath, backupPath); err != nil {
			return 0, err
		}
		updated++
	}
	return updated, nil
}

func pruneEmptyDirs(root string) error {
	var dirs []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("failed to walk %s: %w", path, walkErr)
		}
		if !d.IsDir() || path == root || path == filepath.Join(root, ".git") {
			return nil
		}
		dirs = append(dirs, path)
		return nil
	})
	if err != nil {
		return err
	}

	for i := len(dirs) - 1; i >= 0; i-- {
		entries, err := os.ReadDir(dirs[i])
		if err != nil {
			return fmt.Errorf("failed to read directory %s: %w", dirs[i], err)
		}
		if len(entries) != 0 {
			continue
		}
		if err := os.Remove(dirs[i]); err != nil {
			return fmt.Errorf("failed to remove empty directory %s: %w", dirs[i], err)
		}
	}
	return nil
}

func encryptFile(password, sourcePath, destinationPath string) error {
	data, err := os.ReadFile(sourcePath)
	if err != nil {
		return fmt.Errorf("failed to read source file %s: %w", sourcePath, err)
	}
	block, err := aes.NewCipher(deriveKey(password))
	if err != nil {
		return fmt.Errorf("failed to initialize cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return fmt.Errorf("failed to initialize AEAD: %w", err)
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return fmt.Errorf("failed to generate nonce: %w", err)
	}
	ciphertext := aead.Seal(nil, nonce, data, nil)
	payload := append([]byte{1}, nonce...)
	payload = append(payload, ciphertext...)
	if err := os.MkdirAll(filepath.Dir(destinationPath), 0o700); err != nil {
		return fmt.Errorf("failed to create encrypted parent directory: %w", err)
	}
	if err := os.WriteFile(destinationPath, payload, 0o600); err != nil {
		return fmt.Errorf("failed to write encrypted file %s: %w", destinationPath, err)
	}
	return nil
}

func decryptFile(password, path string) ([]byte, error) {
	payload, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read encrypted file %s: %w", path, err)
	}
	block, err := aes.NewCipher(deriveKey(password))
	if err != nil {
		return nil, fmt.Errorf("failed to initialize cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize AEAD: %w", err)
	}
	if len(payload) < 1+aead.NonceSize() {
		return nil, fmt.Errorf("encrypted file is too short: %s", path)
	}
	if payload[0] != 1 {
		return nil, fmt.Errorf("unsupported encrypted file version %d", payload[0])
	}
	nonce := payload[1 : 1+aead.NonceSize()]
	ciphertext := payload[1+aead.NonceSize():]
	plaintext, err := aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt %s: %w", path, err)
	}
	return plaintext, nil
}

func deriveKey(password string) []byte {
	sum := sha256.Sum256([]byte(password))
	return sum[:]
}

func commitRepo(repo, message string) error {
	changed, err := repoHasChanges(repo)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	if err := runCommand("", "git", "-C", repo, "commit", "-m", message); err != nil {
		return fmt.Errorf("failed to commit repo %s: %w", repo, err)
	}
	return nil
}

func repoHasChanges(repo string) (bool, error) {
	cmd := exec.Command("git", "-C", repo, "status", "--short")
	out, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("failed to inspect repo status: %w", err)
	}
	return strings.TrimSpace(string(out)) != "", nil
}

func listEncryptedFilesFromCommit(repo, ref string) ([]string, error) {
	cmd := exec.Command("git", "-C", repo, "ls-tree", "-r", "--name-only", ref)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to list files from commit %s: %w", ref, err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	files := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasSuffix(line, encryptedSuffix) {
			continue
		}
		files = append(files, line)
	}
	return files, nil
}

func writeFileFromCommit(repo, ref, filePath, destination string) error {
	cmd := exec.Command("git", "-C", repo, "show", ref+":"+filePath)
	data, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("failed to read %s from commit %s: %w", filePath, ref, err)
	}
	if err := os.WriteFile(destination, data, 0o600); err != nil {
		return fmt.Errorf("failed to write temporary file: %w", err)
	}
	return nil
}

func runCommand(dir, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		text := strings.TrimSpace(string(output))
		if text == "" {
			return err
		}
		return fmt.Errorf("%w: %s", err, text)
	}
	return nil
}
