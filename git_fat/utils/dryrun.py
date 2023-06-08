import functools
from typing import Any, Callable


class Dryrun:
    _dryrun = False

    def __init__(
        self,
        return_value: Any = None,
    ):
        self.return_value = return_value
        self.mock_function = None

    def __call__(self, function: Callable) -> Callable:
        @functools.wraps(function)
        def decorator(*args, **kwargs) -> Any:
            if not self._dryrun:
                return function(*args, **kwargs)
            elif self.mock_function:
                return self.mock_function(*args, **kwargs)
            else:
                return self.return_value

        return decorator

    @classmethod
    def set(cls, value: bool):
        cls._dryrun = value


dryrun = Dryrun
