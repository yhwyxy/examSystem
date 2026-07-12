from __future__ import annotations

import ast
import base64
import inspect
import textwrap

from backend.utils import generate_qr_base64


def test_generate_qr_base64_returns_png_data_uri():
    result = generate_qr_base64("https://example.test/exam")

    prefix = "data:image/png;base64,"
    assert result.startswith(prefix)
    assert base64.b64decode(result.removeprefix(prefix)).startswith(b"\x89PNG\r\n\x1a\n")


def test_generate_qr_base64_avoids_dynamic_qrcode_constants_attribute():
    tree = ast.parse(textwrap.dedent(inspect.getsource(generate_qr_base64)))
    dynamic_constants_access = [
        node
        for node in ast.walk(tree)
        if isinstance(node, ast.Attribute)
        and node.attr == "constants"
        and isinstance(node.value, ast.Name)
        and node.value.id == "qrcode"
    ]

    assert dynamic_constants_access == []
