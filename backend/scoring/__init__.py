"""兼容层：转发到独立库 subjective-scoring。

业务代码可继续 `from backend.scoring import ...`，
新代码推荐直接 `from subjective_scoring import ...`。
"""

from subjective_scoring import *  # noqa: F403
from subjective_scoring import __all__ as __all__  # type: ignore
