package dockertest

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"time"
)

// File operations used by simulated remote service APIs live in the same Linux
// filesystem as command execution. Host bind sharing does not preserve all
// chmod/permission semantics on Docker Desktop and must not define these tests.
const fileOperation = `import base64, errno, json, os, shutil, stat, sys
r=json.load(sys.stdin); p=r['path']; op=r['op']
def writable(p):
    if os.path.islink(p): return
    if os.path.isdir(p):
        os.chmod(p,0o700)
        for name in os.listdir(p): writable(os.path.join(p,name))
    elif os.path.exists(p): os.chmod(p,0o600)
try:
    result={}
    if op=='read': result['data']=base64.b64encode(open(p,'rb').read()).decode()
    elif op=='write':
        with open(p,'wb') as f: f.write(base64.b64decode(r.get('data') or ''))
        os.chmod(p,r['mode'])
    elif op=='mkdir': os.mkdir(p,r['mode'])
    elif op=='mkdirall': os.makedirs(p,mode=r['mode'],exist_ok=True)
    elif op=='chmod': os.chmod(p,r['mode'])
    elif op=='symlink': os.symlink(r['target'],p)
    elif op=='remove':
        if os.path.isdir(p) and not os.path.islink(p): os.rmdir(p)
        else: os.unlink(p)
    elif op=='removeall':
        if os.path.islink(p): os.unlink(p)
        elif os.path.exists(p):
            writable(p)
            if os.path.isdir(p): shutil.rmtree(p)
            else: os.unlink(p)
    elif op=='writable': writable(p)
    elif op=='list': result['names']=os.listdir(p)
    elif op in ('stat','lstat'):
        s=os.lstat(p) if op=='lstat' else os.stat(p)
        result.update(name=os.path.basename(p),size=s.st_size,mode=stat.S_IMODE(s.st_mode),directory=stat.S_ISDIR(s.st_mode),symlink=stat.S_ISLNK(s.st_mode))
    else: raise ValueError(op)
    print(json.dumps(result))
except OSError as e: print(json.dumps({'errno':e.errno,'error':str(e)}))
`

type fileReply struct {
	Data      []byte
	Names     []string
	Name      string
	Size      int64
	Mode      fs.FileMode
	Directory bool
	Symlink   bool
	Errno     int
	Error     string
}

func (f *Fixture) file(op, name, target string, mode fs.FileMode, data []byte) (fileReply, error) {
	if name != f.Root && !strings.HasPrefix(path.Clean(name), f.Root+"/") {
		return fileReply{}, fmt.Errorf("test path outside fixture: %s", name)
	}
	body, err := json.Marshal(map[string]any{"op": op, "path": name, "target": target, "mode": uint32(mode.Perm()), "data": data})
	if err != nil {
		return fileReply{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stdout, stderr, code, err := f.Exec(ctx, f.Root, []string{"python3", "-c", fileOperation}, body)
	if err != nil {
		return fileReply{}, err
	}
	if code != 0 {
		return fileReply{}, fmt.Errorf("docker fixture %s: %s", op, stderr)
	}
	var reply fileReply
	if err := json.Unmarshal(stdout, &reply); err != nil {
		return reply, err
	}
	if reply.Error != "" {
		var cause error
		switch reply.Errno {
		case 2:
			cause = fs.ErrNotExist
		case 1, 13:
			cause = fs.ErrPermission
		case 17:
			cause = fs.ErrExist
		default:
			cause = fmt.Errorf("%s", reply.Error)
		}
		return reply, &fs.PathError{Op: op, Path: name, Err: cause}
	}
	return reply, nil
}

func (f *Fixture) ReadFile(name string) ([]byte, error) {
	r, e := f.file("read", name, "", 0, nil)
	return r.Data, e
}
func (f *Fixture) WriteFile(name string, data []byte, mode fs.FileMode) error {
	_, e := f.file("write", name, "", mode, data)
	return e
}
func (f *Fixture) Mkdir(name string, mode fs.FileMode) error {
	_, e := f.file("mkdir", name, "", mode, nil)
	return e
}
func (f *Fixture) MkdirAll(name string, mode fs.FileMode) error {
	_, e := f.file("mkdirall", name, "", mode, nil)
	return e
}
func (f *Fixture) Chmod(name string, mode fs.FileMode) error {
	_, e := f.file("chmod", name, "", mode, nil)
	return e
}
func (f *Fixture) Symlink(target, name string) error {
	_, e := f.file("symlink", name, target, 0, nil)
	return e
}
func (f *Fixture) Remove(name string) error { _, e := f.file("remove", name, "", 0, nil); return e }
func (f *Fixture) RemoveAll(name string) error {
	_, e := f.file("removeall", name, "", 0, nil)
	return e
}
func (f *Fixture) MakeWritable(name string) error {
	_, e := f.file("writable", name, "", 0, nil)
	return e
}
func (f *Fixture) ReadDir(name string) ([]string, error) {
	r, e := f.file("list", name, "", 0, nil)
	return r.Names, e
}
func (f *Fixture) Stat(name string) (fs.FileInfo, error) {
	r, e := f.file("stat", name, "", 0, nil)
	return fixtureInfo{r}, e
}
func (f *Fixture) Lstat(name string) (fs.FileInfo, error) {
	r, e := f.file("lstat", name, "", 0, nil)
	return fixtureInfo{r}, e
}

type fixtureInfo struct{ r fileReply }

func (f fixtureInfo) Name() string { return f.r.Name }
func (f fixtureInfo) Size() int64  { return f.r.Size }
func (f fixtureInfo) Mode() fs.FileMode {
	mode := f.r.Mode
	if f.r.Directory {
		mode |= fs.ModeDir
	}
	if f.r.Symlink {
		mode |= fs.ModeSymlink
	}
	return mode
}
func (f fixtureInfo) ModTime() time.Time { return time.Time{} }
func (f fixtureInfo) IsDir() bool        { return f.r.Directory }
func (f fixtureInfo) Sys() any           { return nil }
