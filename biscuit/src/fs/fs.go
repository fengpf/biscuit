package fs

import "fmt"
import "sync"
import "strconv"
import "common"

const memfs = false // in-memory file system?
const fs_debug = false
const iroot = 0

var cons common.Cons_i

type Fs_t struct {
	ahci         common.Disk_i
	superb_start int
	superb       superblock_t
	bcache       *bcache_t
	icache       *icache_t
	fslog        *log_t
	ialloc       *iallocater_t
	balloc       *ballocater_t
}

func StartFS(mem common.Blockmem_i, disk common.Disk_i, console common.Cons_i) (*common.Fd_t, *Fs_t) {

	cons = console

	fs := &Fs_t{}
	fs.ahci = disk

	if memfs {
		fmt.Printf("Using MEMORY FS\n")
	}

	fs.bcache = mkBcache(mem, disk)
	fs.icache = mkIcache(fs)

	// find the first fs block; the build system installs it in block 0 for
	// us
	b, err := fs.bcache.Get_fill(0, "fsoff", false)
	if err != 0 {
		panic("fs_init")
	}
	FSOFF := 506
	fs.superb_start = common.Readn(b.Data[:], 4, FSOFF)
	fmt.Printf("fs.superb_start %v\n", fs.superb_start)
	if fs.superb_start <= 0 {
		panic("bad superblock start")
	}
	fs.bcache.Relse(b, "fs_init")

	// superblock is never changed, so reading before recovery is fine
	b, err = fs.bcache.Get_fill(fs.superb_start, "super", false) // don't relse b, because superb is global
	if err != 0 {
		panic("fs_init")
	}
	fs.superb = superblock_t{b.Data}

	logstart := fs.superb_start + 1
	loglen := fs.superb.loglen()
	if loglen <= 0 || loglen > 256 {
		panic("bad log len")
	}
	fmt.Printf("logstart %v loglen %v\n", logstart, loglen)
	fs.fslog = StartLog(logstart, loglen, disk, fs.bcache)
	if fs.fslog == nil {
		panic("Startlog failed")
	}

	imapstart := fs.superb.imapblock()
	imaplen := fs.superb.imaplen()
	fmt.Printf("imapstart %v imaplen %v\n", imapstart, imaplen)

	bmapstart := fs.superb.freeblock()
	bmaplen := fs.superb.freeblocklen()
	fmt.Printf("bmapstart %v bmaplen %v\n", bmapstart, bmaplen)

	inodelen := fs.superb.inodelen()
	fmt.Printf("inodestart %v inodelen %v\n", bmapstart+bmaplen, inodelen)

	fs.ialloc = mkIalloc(fs, imapstart, imaplen, bmapstart+bmaplen, inodelen)
	fs.balloc = mkBallocater(fs, bmapstart, bmaplen, bmapstart+bmaplen+inodelen, bmaplen*common.BSIZE)

	return &common.Fd_t{Fops: &fsfops_t{priv: iroot, fs: fs, count: 1}}, fs
}

func (fs *Fs_t) StopFS() {
	fs.fslog.stopLog()
}

func (fs *Fs_t) Fs_statistics() string {
	s := inode_stats()
	if !memfs {
		s += fs.fslog.Stats()
	}
	s += fs.ialloc.Stats()
	s += fs.balloc.Stats()
	s += fs.bcache.Stats()
	s += fs.icache.Stats()
	s += fs.ahci.Stats()
	return s
}

func (fs *Fs_t) Fs_link(old string, new string, cwd common.Inum_t) common.Err_t {
	fs.fslog.Op_begin("Fs_link")
	defer fs.fslog.Op_end()

	if fs_debug {
		fmt.Printf("Fs_link: %v %v %v\n", old, new, cwd)
	}

	istats.nilink++

	orig, err := fs.fs_namei_locked(old, cwd, "Fs_link_org")
	if err != 0 {
		return err
	}
	if orig.itype != I_FILE {
		orig.iunlock_refdown("fs_link")
		return -common.EINVAL
	}
	inum := orig.inum
	orig._linkup()
	orig.iunlock_refdown("fs_link_orig")

	dirs, fn := sdirname(new)
	newd, err := fs.fs_namei_locked(dirs, cwd, "fs_link_newd")
	if err != 0 {
		goto undo
	}
	err = newd.do_insert(fn, inum)
	newd.iunlock_refdown("fs_link_newd")
	if err != 0 {
		goto undo
	}
	return 0
undo:
	orig, err1 := fs.icache.Iref_locked(inum, "fs_link_undo")
	if err1 != 0 {
		panic("bizare")
	}
	orig._linkdown()
	orig.iunlock_refdown("fs_link_undo")
	return err
}

func (fs *Fs_t) Fs_unlink(paths string, cwd common.Inum_t, wantdir bool) common.Err_t {
	fs.fslog.Op_begin("fs_unlink")
	defer fs.fslog.Op_end()

	dirs, fn := sdirname(paths)
	if fn == "." || fn == ".." {
		return -common.EPERM
	}

	istats.nunlink++

	if fs_debug {
		fmt.Printf("fs_unlink: %v cwd %v dir? %v\n", paths, cwd, wantdir)
	}

	var child *imemnode_t
	var par *imemnode_t
	var err common.Err_t
	var childi common.Inum_t

	par, err = fs.fs_namei_locked(dirs, cwd, "fs_unlink_par")
	if err != 0 {
		return err
	}
	childi, err = par.ilookup(fn)
	if err != 0 {
		par.iunlock_refdown("fs_unlink_par")
		return err
	}
	child, err = fs.icache.Iref(childi, "fs_unlink_child")
	if err != 0 {
		par.iunlock_refdown("fs_unlink_par")
		return err
	}
	// unlock parent after we have a ref to child
	par.iunlock("fs_unlink_par")

	// acquire both locks
	iref_lockall([]*imemnode_t{par, child})
	defer par.iunlock_refdown("fs_unlink_par")
	defer child.iunlock_refdown("fs_unlink_child")

	// recheck if child still exists (same name, same inum), since some
	// other thread may have modifed par but par and child won't disappear
	// because we have references to them.
	inum, err := par.ilookup(fn)
	if err != 0 {
		return err
	}
	// name was deleted and recreated?
	if inum != childi {
		return -common.ENOENT
	}

	err = child.do_dirchk(wantdir)
	if err != 0 {
		return err
	}

	// finally, remove directory entry
	err = par.do_unlink(fn)
	if err != 0 {
		return err
	}
	child._linkdown()

	return 0
}

// per-volume rename mutex. Linux does it so it must be OK!
var _renamelock = sync.Mutex{}

func (fs *Fs_t) Fs_rename(oldp, newp string, cwd common.Inum_t) common.Err_t {
	odirs, ofn := sdirname(oldp)
	ndirs, nfn := sdirname(newp)

	if err, ok := crname(ofn, -common.EINVAL); !ok {
		return err
	}
	if err, ok := crname(nfn, -common.EINVAL); !ok {
		return err
	}

	fs.fslog.Op_begin("fs_rename")
	defer fs.fslog.Op_end()

	istats.nrename++

	if fs_debug {
		fmt.Printf("fs_rename: src %v dst %v %v\n", oldp, newp, cwd)
	}

	// one rename at the time
	_renamelock.Lock()
	defer _renamelock.Unlock()

	// lookup all inode references, but we will release locks and lock them
	// together when we know all references.  the references to the inodes
	// cannot disppear, however.
	opar, err := fs.fs_namei_locked(odirs, cwd, "fs_rename_opar")
	if err != 0 {
		return err
	}

	childi, err := opar.ilookup(ofn)
	if err != 0 {
		opar.iunlock_refdown("fs_rename_opar")
		return err
	}
	ochild, err := fs.icache.Iref(childi, "fs_rename_ochild")
	if err != 0 {
		opar.iunlock_refdown("fs_rename_opar")
		return err
	}
	// unlock par after we have ref to child
	opar.iunlock("fs_rename_par")

	npar, err := fs.fs_namei(ndirs, cwd)
	if err != 0 {
		fs.icache.Refdown(opar, "fs_rename_opar")
		fs.icache.Refdown(ochild, "fs_rename_ochild")
		return err
	}

	// verify that ochild is not an ancestor of npar, since we would
	// disconnect ochild subtree from root.  it is safe to do without
	// holding locks because unlink cannot modify the path to the root by
	// removing a directory because the directories aren't empty.  it could
	// delete npar and an ancestor, but rename has already a reference to to
	// npar.
	if err = fs._isancestor(ochild, npar); err != 0 {
		fs.icache.Refdown(opar, "fs_rename_opar")
		fs.icache.Refdown(ochild, "fs_rename_ochild")
		fs.icache.Refdown(npar, "fs_rename_npar")
		return err
	}

	var nchild *imemnode_t
	cnt := 0
	newexists := false
	// lookup newchild and try to lock all inodes involved
	for {
		npar.Lock()
		nchildinum, err := npar.ilookup(nfn)
		if err != 0 && err != -common.ENOENT {
			fs.icache.Refdown(opar, "fs_name_opar")
			fs.icache.Refdown(ochild, "fs_name_ochild")
			npar.iunlock_refdown("fs_name_npar")
			return err
		}
		var err1 common.Err_t
		nchild, err1 = fs.icache.Iref(nchildinum, "fs_rename_ochild")
		if err1 != 0 {
			fs.icache.Refdown(opar, "fs_name_opar")
			fs.icache.Refdown(ochild, "fs_name_ochild")
			npar.iunlock_refdown("fs_name_npar")
			return err
		}
		npar.Unlock()

		var inodes []*imemnode_t
		var locked []*imemnode_t
		if err == 0 {
			newexists = true
			inodes = []*imemnode_t{opar, ochild, npar, nchild}
		} else {
			inodes = []*imemnode_t{opar, ochild, npar}
		}

		locked = iref_lockall(inodes)
		// defers are run last-in-first-out
		for _, v := range inodes {
			defer fs.icache.Refdown(v, "rename")
		}

		for _, v := range locked {
			defer v.iunlock("rename")
		}

		// check if the tree is still the same. an unlink or link may
		// have modified the tree.
		childi, err := opar.ilookup(ofn)
		if err != 0 {
			return err
		}
		// has ofn been removed but a new file ofn has been created?
		if childi != ochild.inum {
			return -common.ENOENT
		}

		childi, err = npar.ilookup(nfn)
		// it existed before and still exists
		if newexists && err == 0 && childi == nchild.inum {
			break
		}
		// it didn't exist before and still doesn't exist
		if !newexists && err == -common.ENOENT {
			break
		}
		// it existed but now it doesn't.
		if newexists && err == -common.ENOENT {
			newexists = false
			nchild.iunlock_refdown("rename_child")
			break
		}

		cnt++
		fmt.Printf("rename: retry %v %v\n", newexists, err)
		if cnt > 100 {
			panic("rename: panic\n")
		}

		// ochildi changed or childi was created; retry, to grab also its lock
		for _, v := range locked {
			v.iunlock("fs_rename_opar")
		}
		if newexists {
			fs.icache.Refdown(nchild, "fs_rename_nchild")
		}
	}

	// if src and dst are the same file, we are done
	if newexists && ochild.inum == nchild.inum {
		return 0
	}

	// guarantee that any page allocations will succeed before starting the
	// operation, which will be messy to piece-wise undo.
	b1, err := npar.probe_insert()
	if err != 0 {
		return err
	}
	defer fs.bcache.Relse(b1, "probe_insert")

	b2, err := opar.probe_unlink(ofn)
	if err != 0 {
		return err
	}
	defer fs.bcache.Relse(b2, "probe_unlink_opar")

	odir := ochild.itype == I_DIR
	if odir {
		b3, err := ochild.probe_unlink("..")
		if err != 0 {
			return err
		}
		defer fs.bcache.Relse(b3, "probe_unlink_ochild")
	}

	if newexists {
		// make sure old and new are either both files or both
		// directories
		if err := nchild.do_dirchk(odir); err != 0 {
			return err
		}

		// remove pre-existing new
		if err = npar.do_unlink(nfn); err != 0 {
			return err
		}
		nchild._linkdown()
	}

	// finally, do the move
	if opar.do_unlink(ofn) != 0 {
		panic("probed")
	}
	if npar.do_insert(nfn, ochild.inum) != 0 {
		panic("probed")
	}

	// update '..'
	if odir {
		if ochild.do_unlink("..") != 0 {
			panic("probed")
		}
		if ochild.do_insert("..", npar.inum) != 0 {
			panic("insert after unlink must succeed")
		}
	}
	return 0
}

// anc and start are in memory
func (fs *Fs_t) _isancestor(anc, start *imemnode_t) common.Err_t {
	if anc.inum == iroot {
		panic("root is always ancestor")
	}
	// walk up to iroot
	here, err := fs.icache.Iref(start.inum, "_isancestor")
	if err != 0 {
		panic("_isancestor: start must exist")
	}
	for here.inum != iroot {
		if anc == here {
			fs.icache.Refdown(here, "_isancestor_here")
			return -common.EINVAL
		}
		here.ilock("_isancestor")
		nexti, err := here.ilookup("..")
		if err != 0 {
			panic(".. must exist")
		}
		if nexti == here.inum {
			here.iunlock("_isancestor")
			panic("xxx")
		} else {
			var next *imemnode_t
			next, err = fs.icache.Iref(nexti, "_isancestor_next")
			here.iunlock_refdown("_isancestor")
			if err != 0 {
				return err
			}
			here = next
		}
	}
	fs.icache.Refdown(here, "_isancestor")
	return 0
}

type fsfops_t struct {
	priv common.Inum_t
	fs   *Fs_t
	// protects offset
	sync.Mutex
	offset int
	append bool
	count  int
}

func (fo *fsfops_t) _read(dst common.Userio_i, toff int) (int, common.Err_t) {
	// lock the file to prevent races on offset and closing
	fo.Lock()
	defer fo.Unlock()

	if fo.count <= 0 {
		return 0, -common.EBADF
	}

	useoffset := toff != -1
	offset := fo.offset
	if useoffset {
		// XXXPANIC
		if toff < 0 {
			panic("neg offset")
		}
		offset = toff
	}
	idm, err := fo.fs.icache.Iref_locked(fo.priv, "_read")
	if err != 0 {
		return 0, err
	}
	did, err := idm.do_read(dst, offset)
	if !useoffset && err == 0 {
		fo.offset += did
	}
	idm.iunlock_refdown("_read")
	return did, err
}

func (fo *fsfops_t) Read(p *common.Proc_t, dst common.Userio_i) (int, common.Err_t) {
	return fo._read(dst, -1)
}

func (fo *fsfops_t) Pread(dst common.Userio_i, offset int) (int, common.Err_t) {
	return fo._read(dst, offset)
}

func (fo *fsfops_t) _write(src common.Userio_i, toff int) (int, common.Err_t) {
	// lock the file to prevent races on offset and closing
	fo.Lock()
	defer fo.Unlock()

	if fo.count <= 0 {
		return 0, -common.EBADF
	}

	useoffset := toff != -1
	offset := fo.offset
	append := fo.append
	if useoffset {
		// XXXPANIC
		if toff < 0 {
			panic("neg offset")
		}
		offset = toff
		append = false
	}
	idm, err := fo.fs.icache.Iref(fo.priv, "_write")
	if err != 0 {
		return 0, err
	}
	did, err := idm.do_write(src, offset, append)
	if !useoffset && err == 0 {
		fo.offset += did
	}
	idm.refdown("_write")
	return did, err
}

func (fo *fsfops_t) Write(p *common.Proc_t, src common.Userio_i) (int, common.Err_t) {
	return fo._write(src, -1)
}

func (fo *fsfops_t) Fullpath() (string, common.Err_t) {
	fo.Lock()
	defer fo.Unlock()

	if fo.count <= 0 {
		return "", -common.EBADF
	}

	fp, err := fo.fs._fullpath(fo.priv)
	return fp, err
}

func (fo *fsfops_t) Truncate(newlen uint) common.Err_t {
	fo.Lock()
	defer fo.Unlock()
	if fo.count <= 0 {
		return -common.EBADF
	}

	fo.fs.fslog.Op_begin("truncate")
	defer fo.fs.fslog.Op_end()

	if fs_debug {
		fmt.Printf("truncate: %v %v\n", fo.priv, newlen)
	}

	idm, err := fo.fs.icache.Iref_locked(fo.priv, "truncate")
	if err != 0 {
		return err
	}
	err = idm.do_trunc(newlen)
	idm.iunlock_refdown("truncate")
	return err
}

func (fo *fsfops_t) Pwrite(src common.Userio_i, offset int) (int, common.Err_t) {
	return fo._write(src, offset)
}

// caller holds fo lock
func (fo *fsfops_t) fstat(st *common.Stat_t) common.Err_t {
	if fs_debug {
		fmt.Printf("fstat: %v %v\n", fo.priv, st)
	}
	idm, err := fo.fs.icache.Iref_locked(fo.priv, "fstat")
	if err != 0 {
		return err
	}
	err = idm.do_stat(st)
	idm.iunlock_refdown("fstat")
	return err
}

func (fo *fsfops_t) Fstat(st *common.Stat_t) common.Err_t {
	fo.Lock()
	defer fo.Unlock()
	if fo.count <= 0 {
		return -common.EBADF
	}
	return fo.fstat(st)
}

// XXX log those files that have no fs links but > 0 memory references to the
// journal so that if we crash before freeing its blocks, the blocks can be
// reclaimed.
func (fo *fsfops_t) Close() common.Err_t {
	fo.Lock()
	defer fo.Unlock()
	if fo.count <= 0 {
		return -common.EBADF
	}
	fo.count--
	if fo.count <= 0 && fs_debug {
		fmt.Printf("Close: %d cnt %d\n", fo.priv, fo.count)

	}
	return fo.fs.Fs_close(fo.priv)
}

func (fo *fsfops_t) Pathi() common.Inum_t {
	// fo.Lock()
	// defer fo.Unlock()
	// if fo.count <= 0 {
	// 	return -common.EBADF
	// }
	return fo.priv
}

func (fo *fsfops_t) Reopen() common.Err_t {
	fo.Lock()
	defer fo.Unlock()
	if fo.count <= 0 {
		return -common.EBADF
	}

	idm, err := fo.fs.icache.Iref_locked(fo.priv, "reopen")
	if err != 0 {
		return err
	}
	istats.nreopen++
	fo.fs.icache.Refup(idm, "reopen") // close will decrease it
	idm.iunlock_refdown("reopen")
	fo.count++
	return 0
}

func (fo *fsfops_t) Lseek(off, whence int) (int, common.Err_t) {
	// prevent races on fo.offset
	fo.Lock()
	defer fo.Unlock()
	if fo.count <= 0 {
		return 0, -common.EBADF
	}

	istats.nlseek++

	switch whence {
	case common.SEEK_SET:
		fo.offset = off
	case common.SEEK_CUR:
		fo.offset += off
	case common.SEEK_END:
		st := &common.Stat_t{}
		fo.fstat(st)
		fo.offset = int(st.Size()) + off
	default:
		return 0, -common.EINVAL
	}
	if fo.offset < 0 {
		fo.offset = 0
	}
	return fo.offset, 0
}

// returns the mmapinfo for the pages of the target file. the page cache is
// populated if necessary.
func (fo *fsfops_t) Mmapi(offset, len int, inc bool) ([]common.Mmapinfo_t, common.Err_t) {
	fo.Lock()
	defer fo.Unlock()
	if fo.count <= 0 {
		return nil, -common.EBADF
	}

	idm, err := fo.fs.icache.Iref_locked(fo.priv, "mmapi")
	if err != 0 {
		return nil, err
	}
	mmi, err := idm.do_mmapi(offset, len, inc)
	idm.iunlock_refdown("mmapi")
	return mmi, err
}

func (fo *fsfops_t) Accept(*common.Proc_t, common.Userio_i) (common.Fdops_i, int, common.Err_t) {
	return nil, 0, -common.ENOTSOCK
}

func (fo *fsfops_t) Bind(*common.Proc_t, []uint8) common.Err_t {
	return -common.ENOTSOCK
}

func (fo *fsfops_t) Connect(proc *common.Proc_t, sabuf []uint8) common.Err_t {
	return -common.ENOTSOCK
}

func (fo *fsfops_t) Listen(*common.Proc_t, int) (common.Fdops_i, common.Err_t) {
	return nil, -common.ENOTSOCK
}

func (fo *fsfops_t) Sendmsg(*common.Proc_t, common.Userio_i, []uint8, []uint8,
	int) (int, common.Err_t) {
	return 0, -common.ENOTSOCK
}

func (fo *fsfops_t) Recvmsg(*common.Proc_t, common.Userio_i,
	common.Userio_i, common.Userio_i, int) (int, int, int, common.Msgfl_t, common.Err_t) {
	return 0, 0, 0, 0, -common.ENOTSOCK
}

func (fo *fsfops_t) Pollone(pm common.Pollmsg_t) (common.Ready_t, common.Err_t) {
	return pm.Events & (common.R_READ | common.R_WRITE), 0
}

func (fo *fsfops_t) Fcntl(proc *common.Proc_t, cmd, opt int) int {
	return int(-common.ENOSYS)
}

func (fo *fsfops_t) Getsockopt(proc *common.Proc_t, opt int, bufarg common.Userio_i,
	intarg int) (int, common.Err_t) {
	return 0, -common.ENOTSOCK
}

func (fo *fsfops_t) Setsockopt(*common.Proc_t, int, int, common.Userio_i, int) common.Err_t {
	return -common.ENOTSOCK
}

func (fo *fsfops_t) Shutdown(read, write bool) common.Err_t {
	return -common.ENOTSOCK
}

type Devfops_t struct {
	Maj int
	Min int
}

func (df *Devfops_t) _sane() {
	// make sure this maj/min pair is handled by Devfops_t. to handle more
	// devices, we can either do dispatch in Devfops_t or we can return
	// device-specific common.Fdops_i in fs_open()
	if df.Maj != common.D_CONSOLE && df.Maj != common.D_DEVNULL && df.Maj != common.D_STAT {
		panic("bad dev")
	}
}

var stats_string = ""

func stat_read(ub common.Userio_i, offset int) (int, common.Err_t) {
	sz := ub.Remain()
	s := stats_string
	if len(s) > sz {
		s = s[:sz]
		stats_string = stats_string[sz:]
	} else {
		stats_string = ""
	}
	kdata := []byte(s)
	ret, err := ub.Uiowrite(kdata)
	if err != 0 || ret != len(kdata) {
		panic("dropped stats")
	}
	return ret, 0
}

func (df *Devfops_t) Read(p *common.Proc_t, dst common.Userio_i) (int, common.Err_t) {
	df._sane()
	if df.Maj == common.D_CONSOLE {
		return cons.Cons_read(dst, 0)
	} else if df.Maj == common.D_STAT {
		return stat_read(dst, 0)
	} else {
		return 0, 0
	}
}

func (df *Devfops_t) Write(p *common.Proc_t, src common.Userio_i) (int, common.Err_t) {
	df._sane()
	if df.Maj == common.D_CONSOLE {
		return cons.Cons_write(src, 0)
	} else {
		return src.Totalsz(), 0
	}
}

func (df *Devfops_t) Fullpath() (string, common.Err_t) {
	panic("weird cwd")
}

func (df *Devfops_t) Truncate(newlen uint) common.Err_t {
	return -common.EINVAL
}

func (df *Devfops_t) Pread(dst common.Userio_i, offset int) (int, common.Err_t) {
	df._sane()
	return 0, -common.ESPIPE
}

func (df *Devfops_t) Pwrite(src common.Userio_i, offset int) (int, common.Err_t) {
	df._sane()
	return 0, -common.ESPIPE
}

func (df *Devfops_t) Fstat(st *common.Stat_t) common.Err_t {
	df._sane()
	st.Wmode(common.Mkdev(df.Maj, df.Min))
	return 0
}

func (df *Devfops_t) Mmapi(int, int, bool) ([]common.Mmapinfo_t, common.Err_t) {
	df._sane()
	return nil, -common.ENODEV
}

func (df *Devfops_t) Pathi() common.Inum_t {
	df._sane()
	panic("bad cwd")
}

func (df *Devfops_t) Close() common.Err_t {
	df._sane()
	return 0
}

func (df *Devfops_t) Reopen() common.Err_t {
	df._sane()
	return 0
}

func (df *Devfops_t) Lseek(int, int) (int, common.Err_t) {
	df._sane()
	return 0, -common.ESPIPE
}

func (df *Devfops_t) Accept(*common.Proc_t, common.Userio_i) (common.Fdops_i, int, common.Err_t) {
	return nil, 0, -common.ENOTSOCK
}

func (df *Devfops_t) Bind(*common.Proc_t, []uint8) common.Err_t {
	return -common.ENOTSOCK
}

func (df *Devfops_t) Connect(proc *common.Proc_t, sabuf []uint8) common.Err_t {
	return -common.ENOTSOCK
}

func (df *Devfops_t) Listen(*common.Proc_t, int) (common.Fdops_i, common.Err_t) {
	return nil, -common.ENOTSOCK
}

func (df *Devfops_t) Sendmsg(*common.Proc_t, common.Userio_i, []uint8, []uint8,
	int) (int, common.Err_t) {
	return 0, -common.ENOTSOCK
}

func (df *Devfops_t) Recvmsg(*common.Proc_t, common.Userio_i,
	common.Userio_i, common.Userio_i, int) (int, int, int, common.Msgfl_t, common.Err_t) {
	return 0, 0, 0, 0, -common.ENOTSOCK
}

func (df *Devfops_t) Pollone(pm common.Pollmsg_t) (common.Ready_t, common.Err_t) {
	switch df.Maj {
	// case common.D_CONSOLE:
	// 	cons.pollc <- pm
	// 	return <- cons.pollret, 0
	// case common.D_DEVNULL:
	// 	return pm.events & (common.R_READ | common.R_WRITE), 0
	default:
		panic("which dev")
	}
}

func (df *Devfops_t) Fcntl(proc *common.Proc_t, cmd, opt int) int {
	return int(-common.ENOSYS)
}

func (df *Devfops_t) Getsockopt(proc *common.Proc_t, opt int, bufarg common.Userio_i,
	intarg int) (int, common.Err_t) {
	return 0, -common.ENOTSOCK
}

func (df *Devfops_t) Setsockopt(*common.Proc_t, int, int, common.Userio_i, int) common.Err_t {
	return -common.ENOTSOCK
}

func (df *Devfops_t) Shutdown(read, write bool) common.Err_t {
	return -common.ENOTSOCK
}

type rawdfops_t struct {
	sync.Mutex
	minor  int
	offset int
	fs     *Fs_t
}

func (raw *rawdfops_t) Read(p *common.Proc_t, dst common.Userio_i) (int, common.Err_t) {
	raw.Lock()
	defer raw.Unlock()
	var did int
	for dst.Remain() != 0 {
		blkno := raw.offset / common.BSIZE
		b, err := raw.fs.fslog.Get_fill(blkno, "read", false)
		if err != 0 {
			return 0, err
		}
		boff := raw.offset % common.BSIZE
		c, err := dst.Uiowrite(b.Data[boff:])
		if err != 0 {
			raw.fs.bcache.Relse(b, "read")
			return 0, err
		}
		raw.offset += c
		did += c
		raw.fs.bcache.Relse(b, "read")
	}
	return did, 0
}

func (raw *rawdfops_t) Write(p *common.Proc_t, src common.Userio_i) (int, common.Err_t) {
	raw.Lock()
	defer raw.Unlock()
	var did int
	for src.Remain() != 0 {
		blkno := raw.offset / common.BSIZE
		boff := raw.offset % common.BSIZE
		// if boff != 0 || src.remain() < 512 {
		//	buf := bdev_read_block(blkno)
		//}
		// XXX don't always have to read block in from disk
		buf, err := raw.fs.fslog.Get_fill(blkno, "write", false)
		if err != 0 {
			return 0, err
		}
		c, err := src.Uioread(buf.Data[boff:])
		if err != 0 {
			raw.fs.bcache.Relse(buf, "write")
			return 0, err
		}
		raw.fs.bcache.Write(buf)
		raw.offset += c
		did += c
		raw.fs.bcache.Relse(buf, "write")
	}
	return did, 0
}

func (raw *rawdfops_t) Fullpath() (string, common.Err_t) {
	panic("weird cwd")
}

func (raw *rawdfops_t) Truncate(newlen uint) common.Err_t {
	return -common.EINVAL
}

func (raw *rawdfops_t) Pread(dst common.Userio_i, offset int) (int, common.Err_t) {
	return 0, -common.ESPIPE
}

func (raw *rawdfops_t) Pwrite(src common.Userio_i, offset int) (int, common.Err_t) {
	return 0, -common.ESPIPE
}

func (raw *rawdfops_t) Fstat(st *common.Stat_t) common.Err_t {
	raw.Lock()
	defer raw.Unlock()
	st.Wmode(common.Mkdev(common.D_RAWDISK, raw.minor))
	return 0
}

func (raw *rawdfops_t) Mmapi(int, int, bool) ([]common.Mmapinfo_t, common.Err_t) {
	return nil, -common.ENODEV
}

func (raw *rawdfops_t) Pathi() common.Inum_t {
	panic("bad cwd")
}

func (raw *rawdfops_t) Close() common.Err_t {
	return 0
}

func (raw *rawdfops_t) Reopen() common.Err_t {
	return 0
}

func (raw *rawdfops_t) Lseek(off, whence int) (int, common.Err_t) {
	raw.Lock()
	defer raw.Unlock()

	switch whence {
	case common.SEEK_SET:
		raw.offset = off
	case common.SEEK_CUR:
		raw.offset += off
	//case common.SEEK_END:
	default:
		return 0, -common.EINVAL
	}
	if raw.offset < 0 {
		raw.offset = 0
	}
	return raw.offset, 0
}

func (raw *rawdfops_t) Accept(*common.Proc_t, common.Userio_i) (common.Fdops_i, int, common.Err_t) {
	return nil, 0, -common.ENOTSOCK
}

func (raw *rawdfops_t) Bind(*common.Proc_t, []uint8) common.Err_t {
	return -common.ENOTSOCK
}

func (raw *rawdfops_t) Connect(proc *common.Proc_t, sabuf []uint8) common.Err_t {
	return -common.ENOTSOCK
}

func (raw *rawdfops_t) Listen(*common.Proc_t, int) (common.Fdops_i, common.Err_t) {
	return nil, -common.ENOTSOCK
}

func (raw *rawdfops_t) Sendmsg(*common.Proc_t, common.Userio_i, []uint8, []uint8,
	int) (int, common.Err_t) {
	return 0, -common.ENOTSOCK
}

func (raw *rawdfops_t) Recvmsg(*common.Proc_t, common.Userio_i,
	common.Userio_i, common.Userio_i, int) (int, int, int, common.Msgfl_t, common.Err_t) {
	return 0, 0, 0, 0, -common.ENOTSOCK
}

func (raw *rawdfops_t) Pollone(pm common.Pollmsg_t) (common.Ready_t, common.Err_t) {
	return pm.Events & (common.R_READ | common.R_WRITE), 0
}

func (raw *rawdfops_t) Fcntl(proc *common.Proc_t, cmd, opt int) int {
	return int(-common.ENOSYS)
}

func (raw *rawdfops_t) Getsockopt(proc *common.Proc_t, opt int, bufarg common.Userio_i,
	intarg int) (int, common.Err_t) {
	return 0, -common.ENOTSOCK
}

func (raw *rawdfops_t) Setsockopt(*common.Proc_t, int, int, common.Userio_i, int) common.Err_t {
	return -common.ENOTSOCK
}

func (raw *rawdfops_t) Shutdown(read, write bool) common.Err_t {
	return -common.ENOTSOCK
}

func (fs *Fs_t) Fs_mkdir(paths string, mode int, cwd common.Inum_t) common.Err_t {
	fs.fslog.Op_begin("fs_mkdir")
	defer fs.fslog.Op_end()

	istats.nmkdir++

	if fs_debug {
		fmt.Printf("mkdir: %v %v\n", paths, cwd)
	}

	dirs, fn := sdirname(paths)
	if err, ok := crname(fn, -common.EINVAL); !ok {
		return err
	}
	if len(fn) > DNAMELEN {
		return -common.ENAMETOOLONG
	}

	par, err := fs.fs_namei_locked(dirs, cwd, "mkdir")
	if err != 0 {
		return err
	}
	defer par.iunlock_refdown("fs_mkdir_par")

	var childi common.Inum_t
	childi, err = par.do_createdir(fn)
	if err != 0 {
		return err
	}

	child, err := fs.icache.Iref(childi, "fs_mkdir_child")
	if err != 0 {
		par.create_undo(childi, fn)
		return err
	}

	child.do_insert(".", childi)
	child.do_insert("..", par.inum)
	fs.icache.Refdown(child, "fs_mkdir3")
	return 0
}

// a type to represent on-disk files
type Fsfile_t struct {
	Inum  common.Inum_t
	Major int
	Minor int
}

func (fs *Fs_t) Fs_open_inner(paths string, flags common.Fdopt_t, mode int, cwd common.Inum_t, major, minor int) (Fsfile_t, common.Err_t) {
	trunc := flags&common.O_TRUNC != 0
	creat := flags&common.O_CREAT != 0
	nodir := false

	if fs_debug {
		fmt.Printf("fs_open: %v %v %v\n", paths, cwd, creat)
	}

	// open with O_TRUNC is not read-only
	if trunc || creat {
		fs.fslog.Op_begin("fs_open")
		defer fs.fslog.Op_end()
	}
	var ret Fsfile_t
	var idm *imemnode_t
	if creat {
		nodir = true
		// creat w/execl; must atomically create and open the new file.
		isdev := major != 0 || minor != 0

		// must specify at least one path component
		dirs, fn := sdirname(paths)
		if err, ok := crname(fn, -common.EEXIST); !ok {
			return ret, err
		}

		if len(fn) > DNAMELEN {
			return ret, -common.ENAMETOOLONG
		}

		var exists bool
		// with O_CREAT, the file may exist. use itrylock and
		// unlock/retry to avoid deadlock.
		for {
			par, err := fs.fs_namei_locked(dirs, cwd, "Fs_open_inner")
			if err != 0 {
				return ret, err
			}
			defer par.iunlock_refdown("Fs_open_inner_par")

			var childi common.Inum_t
			if isdev {
				childi, err = par.do_createnod(fn, major, minor)
			} else {
				childi, err = par.do_createfile(fn)
			}
			if err != 0 && err != -common.EEXIST {
				return ret, err
			}
			exists = err == -common.EEXIST
			if childi <= 0 {
				panic("non-positive childi\n")
			}
			idm, err = fs.icache.Iref_locked(childi, "Fs_open_inner_child")
			if err != 0 {
				par.create_undo(childi, fn)
				return ret, err
			}
			break
		}
		oexcl := flags&common.O_EXCL != 0
		if exists {
			if oexcl || isdev {
				idm.iunlock_refdown("Fs_open_inner2")
				return ret, -common.EEXIST
			}
		}
	} else {
		// open existing file
		var err common.Err_t
		idm, err = fs.fs_namei_locked(paths, cwd, "Fs_open_inner_existing")
		if err != 0 {
			return ret, err
		}
		// idm is locked
	}
	defer idm.iunlock_refdown("Fs_open_inner_idm")

	itype := idm.itype

	o_dir := flags&common.O_DIRECTORY != 0
	wantwrite := flags&(common.O_WRONLY|common.O_RDWR) != 0
	if wantwrite {
		nodir = true
	}

	// verify flags: dir cannot be opened with write perms and only dir can
	// be opened with O_DIRECTORY
	if o_dir || nodir {
		if o_dir && itype != I_DIR {
			return ret, -common.ENOTDIR
		}
		if nodir && itype == I_DIR {
			return ret, -common.EISDIR
		}
	}

	if nodir && trunc {
		idm.do_trunc(0)
	}

	fs.icache.Refup(idm, "Fs_open_inner")

	ret.Inum = idm.inum
	ret.Major = idm.major
	ret.Minor = idm.minor
	return ret, 0
}

// socket files cannot be open(2)'ed (must use connect(2)/sendto(2) etc.)
var _denyopen = map[int]bool{common.D_SUD: true, common.D_SUS: true}

func (fs *Fs_t) Fs_open(paths string, flags common.Fdopt_t, mode int, cwd common.Inum_t, major, minor int) (*common.Fd_t, common.Err_t) {
	istats.nopen++
	fsf, err := fs.Fs_open_inner(paths, flags, mode, cwd, major, minor)
	if err != 0 {
		return nil, err
	}

	// some special files (sockets) cannot be opened with fops this way
	if denied := _denyopen[fsf.Major]; denied {
		if fs.Fs_close(fsf.Inum) != 0 {
			panic("must succeed")
		}
		return nil, -common.EPERM
	}

	// convert on-disk file to fd with fops
	priv := fsf.Inum
	maj := fsf.Major
	min := fsf.Minor
	ret := &common.Fd_t{}
	if maj != 0 {
		// don't need underlying file open
		if fs.Fs_close(fsf.Inum) != 0 {
			panic("must succeed")
		}
		switch maj {
		case common.D_CONSOLE, common.D_DEVNULL, common.D_STAT:
			if maj == common.D_STAT {
				stats_string = fs.Fs_statistics()
			}
			ret.Fops = &Devfops_t{Maj: maj, Min: min}
		case common.D_RAWDISK:
			ret.Fops = &rawdfops_t{minor: min, fs: fs}
		default:
			panic("bad dev")
		}
	} else {
		apnd := flags&common.O_APPEND != 0
		ret.Fops = &fsfops_t{priv: priv, fs: fs, append: apnd, count: 1}
	}
	return ret, 0
}

func (fs *Fs_t) Fs_close(priv common.Inum_t) common.Err_t {
	fs.fslog.Op_begin("Fs_close")
	defer fs.fslog.Op_end()

	istats.nclose++

	if fs_debug {
		fmt.Printf("Fs_close: %v\n", priv)
	}

	idm, err := fs.icache.Iref_locked(priv, "Fs_close")
	if err != 0 {
		return err
	}
	fs.icache.Refdown(idm, "Fs_close")
	idm.iunlock_refdown("Fs_close")
	return 0
}

func (fs *Fs_t) Fs_stat(path string, st *common.Stat_t, cwd common.Inum_t) common.Err_t {
	if fs_debug {
		fmt.Printf("fstat: %v %v\n", path, cwd)
	}
	idm, err := fs.fs_namei_locked(path, cwd, "Fs_stat")
	if err != 0 {
		return err
	}
	err = idm.do_stat(st)
	idm.iunlock_refdown("Fs_stat")
	return err
}

func (fs *Fs_t) Fs_sync() common.Err_t {
	if memfs {
		return 0
	}
	istats.nsync++
	fs.fslog.Force()
	return 0
}

// if the path resolves successfully, returns the idaemon locked. otherwise,
// locks no idaemon.
func (fs *Fs_t) fs_namei(paths string, cwd common.Inum_t) (*imemnode_t, common.Err_t) {
	var start *imemnode_t
	var err common.Err_t
	istats.nnamei++
	if len(paths) == 0 || paths[0] != '/' {
		start, err = fs.icache.Iref(cwd, "fs_namei_cwd")
		if err != 0 {
			panic("cannot load cwd")
		}
	} else {
		start, err = fs.icache.Iref(iroot, "fs_namei_root")
		if err != 0 {
			panic("cannot load iroot")
		}
	}
	idm := start
	pp := pathparts_t{}
	pp.pp_init(paths)
	for cp, ok := pp.next(); ok; cp, ok = pp.next() {
		idm.ilock("fs_namei")
		n, err := idm.ilookup(cp)
		if err != 0 {
			idm.iunlock_refdown("fs_namei_ilookup")
			return nil, err
		}
		if n != idm.inum {
			next, err := fs.icache.Iref(n, "fs_namei_next")
			idm.iunlock_refdown("fs_namei_idm")
			if err != 0 {
				return nil, err
			}
			idm = next
		} else {
			idm.iunlock("fs_namei_idm_next")
		}
	}
	return idm, 0
}

func (fs *Fs_t) fs_namei_locked(paths string, cwd common.Inum_t, s string) (*imemnode_t, common.Err_t) {
	idm, err := fs.fs_namei(paths, cwd)
	if err != 0 {
		return nil, err
	}
	idm.ilock(s + "/fs_namei_locked")
	return idm, 0
}

// superblock format (see mkbdisk.py)
type superblock_t struct {
	data *common.Bytepg_t
}

func (sb *superblock_t) loglen() int {
	return fieldr(sb.data, 0)
}

func (sb *superblock_t) imapblock() int {
	return fieldr(sb.data, 1)
}

func (sb *superblock_t) imaplen() int {
	return fieldr(sb.data, 2)
}

func (sb *superblock_t) freeblock() int {
	return fieldr(sb.data, 3)
}

func (sb *superblock_t) freeblocklen() int {
	return fieldr(sb.data, 4)
}

func (sb *superblock_t) inodelen() int {
	return fieldr(sb.data, 5)
}

func (sb *superblock_t) lastblock() int {
	return fieldr(sb.data, 6)
}

// Bitmap allocater. Used for inodes and blocks
type allocater_t struct {
	sync.Mutex

	fs        *Fs_t
	freestart int
	freelen   int
	lastblk   int
	lastbyte  int

	// stats
	nalloc int
	nfree  int
	nhit   int
}

func mkAllocater(fs *Fs_t, start, len int) *allocater_t {
	a := &allocater_t{}
	a.fs = fs
	a.freestart = start
	a.freelen = len
	return a
}

func freebit(b uint8) uint {
	for m := uint(0); m < 8; m++ {
		if (1<<m)&b == 0 {
			return m
		}
	}
	panic("no 0 bit?")
}

func (alloc *allocater_t) Fbread(blockno int) (*common.Bdev_block_t, common.Err_t) {
	if blockno < alloc.freestart || blockno >= alloc.freestart+alloc.freelen {
		panic("naughty blockno")
	}
	return alloc.fs.fslog.Get_fill(blockno, "fbread", true)
}

// 0 is free, 1 is allocated
func (alloc *allocater_t) search() (*common.Bdev_block_t, int, int, uint, common.Err_t) {
	var blk *common.Bdev_block_t
	var err common.Err_t
	for b := 0; b < alloc.freelen; b++ {
		if blk != nil {
			blk.Unlock()
			alloc.fs.bcache.Relse(blk, "alloc search")
		}
		blk, err = alloc.Fbread(alloc.freestart + b)
		if err != 0 {
			return nil, 0, 0, 0, err
		}
		for idx := 0; idx < len(blk.Data); idx++ {
			c := blk.Data[idx]
			if c != 0xff {
				alloc.lastblk = b
				alloc.lastbyte = idx
				bit := freebit(c)
				return blk, b, idx, bit, 0
			}
		}
	}
	return nil, 0, 0, 0, -common.ENOMEM
}

func (alloc *allocater_t) quickcheck() (*common.Bdev_block_t, int, int, uint, common.Err_t) {
	blk, err := alloc.Fbread(alloc.freestart + alloc.lastblk)
	if err != 0 {
		return nil, 0, 0, 0, err
	}
	c := blk.Data[alloc.lastbyte]
	if c != 0xff {
		bit := freebit(c)
		blkn := alloc.lastblk
		byte := alloc.lastbyte
		if c == 0x7f {
			alloc.lastbyte = (alloc.lastbyte + 1) % common.BSIZE
			if alloc.lastbyte == 0 {
				alloc.lastblk = (alloc.lastblk + 1) % alloc.freelen

			}
		}
		return blk, blkn, byte, bit, 0
	}
	blk.Unlock()
	alloc.fs.bcache.Relse(blk, "alloc quickcheck")
	return nil, 0, 0, 0, -common.ENOMEM
}

func (alloc *allocater_t) Alloc() (int, common.Err_t) {
	alloc.Lock()
	defer alloc.Unlock()

	blk, blkn, oct, bit, err := alloc.quickcheck()
	if err == 0 {
		alloc.nhit++
	} else {
		blk, blkn, oct, bit, err = alloc.search()
		if err != 0 {
			return 0, err
		}
	}
	alloc.nalloc++

	// mark as allocated
	blk.Data[oct] |= 1 << bit
	blk.Unlock()
	alloc.fs.fslog.Write(blk)
	alloc.fs.bcache.Relse(blk, "Alloc")

	bitsperblk := common.BSIZE * 8
	blkn = blkn*bitsperblk + oct*8 + int(bit)
	return blkn, 0
}

func (alloc *allocater_t) Free(blkno int) common.Err_t {
	alloc.Lock()
	defer alloc.Unlock()

	if fs_debug {
		fmt.Printf("bfree: %v\n", blkno)
	}

	if blkno < 0 {
		panic("free bad blockno")
	}

	bit := blkno
	bitsperblk := common.BSIZE * 8
	fblkno := alloc.freestart + bit/bitsperblk
	fbit := bit % bitsperblk
	fbyteoff := fbit / 8
	fbitoff := uint(fbit % 8)
	if fblkno >= alloc.freestart+alloc.freelen {
		panic("free: bad blockno")
	}
	fblk, err := alloc.Fbread(fblkno)
	if err != 0 {
		return err
	}
	fblk.Data[fbyteoff] &= ^(1 << fbitoff)
	fblk.Unlock()
	alloc.fs.fslog.Write(fblk)
	alloc.fs.bcache.Relse(fblk, "free")
	alloc.nfree++
	return 0
}

func (alloc *allocater_t) Stats() string {
	s := "allocater: #alloc "
	s += strconv.Itoa(alloc.nalloc)
	s += " #free "
	s += strconv.Itoa(alloc.nfree)
	s += " #hit "
	s += strconv.Itoa(alloc.nhit)
	s += "\n"
	return s
}
