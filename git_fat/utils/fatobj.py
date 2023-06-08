from pathlib import Path


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
