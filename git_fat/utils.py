from git.repo import Repo
from git import Commit
from git.objects.base import Object as Gobject
from functools import singledispatchmethod
import git.objects
from pathlib import Path
from git_fat.fatstores import S3FatStore
import hashlib
from typing import List, Set, Tuple, IO, Union
import tomli
import tempfile
import os
import sys
import shutil

BLOCK_SIZE = 4096


def umask():
    """Get umask without changing it."""
    old = os.umask(0)
    os.umask(old)
    return old


def tostr(s, encoding="utf-8") -> str:
    """Automate unicode conversion"""
    if isinstance(s, str):
        return s
    if hasattr(s, "decode"):
        return s.decode(encoding)
    raise ValueError("Cound not decode")


def tobytes(s, encoding="utf8") -> bytes:
    """Automatic byte conversion"""
    if isinstance(s, bytes):
        return s
    if hasattr(s, "encode"):
        return s.encode(encoding)
    raise ValueError("Could not encode")


class NoArgs:
    pass


class FatObj:
    def __init__(self, path: Path, fatid: str, size: int, working_dir: Path):
        self.fatid = fatid
        self.path = str(path.relative_to(working_dir))
        self.opath = path
        self.abspath = str(path.absolute())
        self.working_dir = working_dir
        self.size = size

    def __eq__(self, other):
        return (
            isinstance(other, self.__class__)
            and getattr(other, "fatid", None) == self.fatid
            and getattr(other, "spath", None) == self.path
        )

    def __hash__(self):
        return hash(self.fatid + self.path)


class FatRepo:
    def __init__(self, directory: Path):
        self.gitapi = Repo(str(directory), search_parent_directories=True)
        self.workspace = Path(directory)
        self.gitfat_config_path = self.workspace / ".gitfat"
        self.magiclen = self.get_magiclen()
        self.cookie = b"#$# git-fat"
        self.objdir = self.workspace / ".git" / "fat/objects"
        self.debug = True if os.environ.get("GIT_FAT_VERBOSE") else False
        self._gitfat_config = None
        self._fatstore = None
        self._smudgestore = None
        self.setup()

    @property
    def gitfat_config(self):
        if not self._gitfat_config:
            self._gitfat_config = self.get_gitfat_config()
        return self._gitfat_config

    @property
    def fatstore(self):
        if not self._fatstore:
            self._fatstore = self.get_fatstore()
        return self._fatstore

    @property
    def smudgestore(self):
        if not self._smudgestore:
            self._smudgestore = self.get_smudgestore()
        return self._smudgestore

    def verbose(self, *args, force: bool = False, **kargs):
        if force or self.debug:
            print(*args, file=sys.stderr, **kargs)

    def encode_fatstub(self, digest: str, size: float) -> str:
        """
        Returns a string containg the git-fat stub of a file cleaned with the git-fat filter.
        I.E. #$# git-fat file_hex_digest file_size
            Parameters:
                sha_digest (str): sha Sum of file
                size (float): Size of file in bytes
        """
        return "#$# git-fat %s %20d\n" % (digest, size)

    def decode_fatstub(self, string: str) -> Tuple[str, int]:
        """
        Returns the fatid (sha1 digest) and size of a file that's been smudged by the git-fat filter
            Parameters:
                string: Git fat stub string
        """

        parts = string[len(self.cookie) :].split()
        fatid = parts[0]
        size = int(parts[1]) if len(parts) > 1 else 0
        return fatid, size

    def get_magiclen(self) -> int:
        """
        Returns an interger that is equal to the length of the git-fat stub (74)
        """
        dummy_file_contents = b"dummy"
        dummy_file_sha = hashlib.sha1(b"dummy").hexdigest()
        dummy_file_size = len(dummy_file_contents)
        return len(self.encode_fatstub(dummy_file_sha, dummy_file_size))

    def get_gitfat_config(self) -> dict:
        """
        Returns a directory of gitfat config in repo
        """
        if not self.gitfat_config_path.exists():
            self.verbose("No valid fat config exists", force=True)
            sys.exit(1)

        with open(self.gitfat_config_path, "rb") as f:
            gitfat_config = tomli.load(f)

        return gitfat_config

    def get_fatstore_type(self) -> str:
        """
        Returns first section name from gitfat config
        """
        config_keys = list(self.gitfat_config.keys())
        return config_keys[0]

    def get_smudgestore(self):
        """
        Returns initialize smudge store as described in gitfat config
        """
        fatstore_type = self.get_fatstore_type()
        config = dict(self.gitfat_config[fatstore_type]["smudgestore"])
        return S3FatStore(config)

    def get_fatstore(self):
        """
        Returns initialize fatstore as described in gitfat config
        """
        fatstore_type = self.get_fatstore_type()
        config = self.gitfat_config[fatstore_type]
        return S3FatStore(config)

    def is_fatblob(self, item: Gobject):
        """
        Takes GitPython object, returns true if Blob and datastream starts with git-fat cookie
        """
        if item.type != "blob":
            return False

        fatstub_candidate = item.data_stream.read(self.magiclen)
        return self.is_fatstub(fatstub_candidate)

    def get_all_git_references(self) -> List[str]:
        return [str(ref) for ref in self.gitapi.refs]

    def create_fatobj(self, blob: git.objects.Blob) -> FatObj:
        fatid, size = self.decode_fatstub(blob.data_stream.read())
        fatobj_path = Path(blob.abspath)

        return FatObj(path=fatobj_path, fatid=tostr(fatid), size=size, working_dir=Path(self.workspace))

    def get_indexed_fatobjs(self) -> Set[FatObj]:
        """
        Returns a filtered list of GitPython blob objects categorized as git-fat blobs.
        see: https://gitpython.readthedocs.io/en/stable/reference.html?highlight=size#module-git.objects.base
        """
        unique_fatobjs = set()

        index = self.gitapi.index
        unique_fatobjs = {
            self.create_fatobj(blob) for stage, blob in index.iter_blobs() if stage == 0 and self.is_fatblob(blob)
        }
        return unique_fatobjs

    def is_gitfat_initialized(self) -> bool:
        with self.gitapi.config_reader() as cr:
            return cr.has_section('filter "fat"')

    def setup(self):
        if not self.objdir.exists():
            self.objdir.mkdir(mode=0o755, parents=True)

        if not self.is_gitfat_initialized():
            with self.gitapi.config_writer() as cw:
                cw.set_value('filter "fat"', "clean", "git fat filter-clean")
                cw.set_value('filter "fat"', "smudge", "git fat filter-smudge")

    def is_fatstub(self, data: bytes) -> bool:
        cookie = data[: len(self.cookie)]
        if len(data) != self.magiclen:
            return False
        return cookie == self.cookie

    def cache_fatfile(self, cached_file: str, file_sha_digest: str):
        objfile = self.objdir / file_sha_digest
        if objfile.exists():
            self.verbose(f"git-fat: cache already exists {objfile}")
            os.remove(cached_file)
            return

        # Set permissions for the new file using the current umask
        os.chmod(cached_file, int("444", 8) & ~umask())
        os.rename(cached_file, objfile)
        self.verbose(f"git-fat filter-clean: caching to {objfile.relative_to(self.workspace)}")

    def filter_clean(self, input_handle: IO, output_handle: IO):
        """
        Takes IO byte stream (input_handle), writes git-fat file stub (sha-magic) bytes on output_handle
        """
        first_block = tobytes(input_handle.read(BLOCK_SIZE))
        if self.is_fatstub(first_block):
            output_handle.write(first_block)
            return

        fd, tmpfile_path = tempfile.mkstemp(dir=self.objdir)
        sha = hashlib.new("sha1")
        sha.update(first_block)
        fat_size = len(first_block)

        with os.fdopen(fd, "wb") as tmpfile_handle:
            tmpfile_handle.write(first_block)
            while True:
                block = tobytes(input_handle.read(BLOCK_SIZE))
                if not block:
                    break
                sha.update(block)
                fat_size += len(block)
                tmpfile_handle.write(block)
            tmpfile_handle.flush()

        sha_digest = sha.hexdigest()
        self.cache_fatfile(tmpfile_path, sha_digest)
        fatstub = self.encode_fatstub(sha_digest, fat_size)
        # output clean bytes (fatstub) to output_handle
        output_handle.write(tobytes(fatstub))

    def filter_smudge(self, input_handle: IO, output_handle: IO):
        """
        Takes IO byte stream (git-fat file stub), writes full file contents on output_handle
        """
        fatstub_candidate = input_handle.read(self.magiclen)
        if not self.is_fatstub(fatstub_candidate):
            self.verbose("Not a git-fat object")
            self.verbose("git-fat filter-smudge: fat stub not found in input stream")
            return

        sha_digest, size = self.decode_fatstub(fatstub_candidate)
        fatfile = self.objdir / tostr(sha_digest)
        if not fatfile.exists:
            self.verbose("git-fat filter-smudge: fat object missing, maybe pull?")
            return

        read_size = 0
        with open(fatfile, "rb") as fatfile_handle:
            while True:
                block = fatfile_handle.read(BLOCK_SIZE)
                if not block:
                    break
                output_handle.write(block)
                read_size += len(block)

        relative_obj = fatfile.relative_to(self.workspace)
        if read_size != size:
            self.verbose(
                f"git-fat filter-smudge: invalid file size of {relative_obj}, expected: {size}, got: {read_size}",
                force=True,
            )

    def restore_fatobj(self, obj: FatObj):
        cache = self.objdir / obj.fatid
        self.verbose(f"git-fat pull: restore {obj.path} from {cache.name}", force=True)
        stat = os.lstat(obj.abspath)
        shutil.copy(self.objdir / obj.fatid, obj.abspath)
        os.chmod(obj.abspath, stat.st_mode)
        os.utime(obj.abspath, (stat.st_atime, stat.st_mtime))
        self.gitapi.git.execute(
            command=["git", "update-index", obj.abspath],
            stdout_as_string=True,
        )

    def pull_all(self):
        local_fatfiles = os.listdir(self.objdir)
        remote_fatfiles = self.fatstore.list()
        idx_fatobjs = self.get_indexed_fatobjs()

        pull_candidates = [file for file in remote_fatfiles if file not in local_fatfiles]
        if len(pull_candidates) == 0:
            self.verbose("git-fat pull: nothing to pull", force=True)
            return

        for obj in idx_fatobjs:
            if obj.fatid not in pull_candidates or obj.fatid not in remote_fatfiles:
                self.verbose(f"git-fat pull: {obj.path} found locally, skipping", force=True)
                continue
            self.verbose(f"git-fat pull: downloading {obj.fatid}")
            self.fatstore.download(obj.fatid, self.objdir / obj.fatid)
            self.restore_fatobj(obj)

    def pull(self, files: List[Path] = []):
        if len(files) == 0:
            self.verbose("git-fat pull: nothing to pull", force=True)
            return

        for fpath in files:
            try:
                rpath = fpath.relative_to(self.gitapi.working_dir)  # type: ignore
                blob = self.gitapi.tree() / str(rpath)
                if not self.is_fatblob(blob):
                    self.verbose(f"git-fat pull: {rpath} is not a fat object", force=True)
                    continue
                obj = self.create_fatobj(blob)  # type: ignore
                self.verbose(f"git-fat pull: pulling {obj.fatid} to {obj.path}", force=True)
                self.fatstore.download(obj.fatid, self.objdir / obj.fatid)
                self.restore_fatobj(obj)
            except KeyError:
                self.verbose(f"git-fat pull: {fpath} not found in repo", force=True)

    def push_fatobjs(self, objects: List[FatObj]):
        if len(objects) == 0:
            self.verbose("git-fat push: nothing to push", force=True)
            return

        for obj in objects:
            self.verbose(f"git-fat push: uploading {obj.path}", force=True)
            self.fatstore.upload(str(self.objdir / obj.fatid))

    def push(self):
        self.setup()
        local_fatfiles = os.listdir(self.objdir)
        remote_fatfiles = self.fatstore.list()
        idx_fatojbs = self.get_indexed_fatobjs()

        push_candidates = [fatobj for fatobj in idx_fatojbs if fatobj.fatid in local_fatfiles]
        if len(push_candidates) == 0:
            self.verbose("git-fat push: nothing to push", force=True)
            return

        needs_pushing = [fatobj for fatobj in push_candidates if fatobj.fatid not in remote_fatfiles]
        self.push_fatobjs(needs_pushing)

    def confirm_on_remote(self, search_list: Set[FatObj]) -> None:
        remote_fatfiles = self.fatstore.list()
        missing_fatobjs = [fatobj for fatobj in search_list if fatobj.fatid not in remote_fatfiles]
        if len(missing_fatobjs) != 0:
            for missing_obj in missing_fatobjs:
                self.verbose(f"git-fat: {missing_obj.path} not found on remote store", force=True)
            sys.exit(1)

    def get_added_fatobjs(self, base: Commit, ref: Union[None, Commit] = None) -> Set[FatObj]:
        """
        Compares given commit (base) with given REF or working index and returns set of FatObj
        """
        diff_index = base.diff(ref)
        added_fatobjs = set()
        for diff_item in diff_index.iter_change_type("A"):
            new_blob = diff_item.b_blob
            if not self.is_fatblob(new_blob):
                continue
            added_fatobjs.add(self.create_fatobj(new_blob))
        return added_fatobjs

    @singledispatchmethod
    def fatstore_check(self, arg):
        raise NotImplementedError(f"Cannot format value of type {type(arg)}")

    @fatstore_check.register(list)
    def _(
        self, files  # type: List[Path]
    ) -> None:
        if len(files) == 0:
            return
        fatobjs = self.get_indexed_fatobjs()
        requested_abspaths = [str(fpath.absolute()) for fpath in files]
        fatobjs_to_find = {o for o in fatobjs if o.abspath in requested_abspaths}
        self.confirm_on_remote(fatobjs_to_find)

    @fatstore_check.register(NoArgs)
    def _(self, _) -> None:
        fatobjs = self.get_indexed_fatobjs()
        self.confirm_on_remote(fatobjs)

    @fatstore_check.register(Commit)
    def _(
        self, branch  # type: Commit
    ) -> None:
        added_blobs = self.get_added_fatobjs(branch)
        self.confirm_on_remote(added_blobs)

    def publish_added_fatobjs(self, ref: Commit) -> None:
        """
        Takes REF, finds new fatobjs in REF but not in HEAD and uploads to smudge store
        """
        head = self.gitapi.head.commit
        added_fatobjs = self.get_added_fatobjs(ref, head)
        for fatobj in added_fatobjs:
            fpath = Path(fatobj.abspath)
            keyname = str(fpath.relative_to(self.workspace))
            fatobj_cache_path = self.objdir / fatobj.fatid
            if not fatobj_cache_path.exists():
                self.pull(files=[fpath])
            self.verbose(f"git-fat: publishing '{keyname}' to smudgestore", force=True)
            self.smudgestore.upload(local_filename=str(fatobj_cache_path), remote_filename=keyname)

    # def status(self):
    #     pass
