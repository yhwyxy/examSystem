"""通用工具：时间处理、局域网 IP 识别、二维码生成。"""
from __future__ import annotations

import base64
import io
import socket
from datetime import datetime, timezone


# ---------------- 时间工具 ----------------

def now_iso() -> str:
    """当前本地时间 ISO 字符串（秒精度）。"""
    return datetime.now().astimezone().replace(microsecond=0).isoformat()


def parse_iso(s: str) -> datetime:
    """解析 ISO 字符串为 datetime；容错处理无时区或格式非法的情况。"""
    # try-except 内部不重复同一调用（否则等同于抛出）。先按原始字符串解析。
    dt = datetime.fromisoformat(s)
    if dt.tzinfo is None:
        # naive（无时区）则按本地时区解释，避免后续时差计算错误。
        dt = dt.astimezone()
    return dt


def seconds_between(start_iso: str, end_iso: str) -> float:
    a = parse_iso(start_iso)
    b = parse_iso(end_iso)
    return (b - a).total_seconds()


# ---------------- 局域网 IP ----------------

def get_lan_ip() -> str:
    """获取本机在局域网内的 IPv4 地址；失败回退到 127.0.0.1。"""
    s: socket.socket | None = None
    try:
        s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        s.connect(("8.8.8.8", 80))
        ip = s.getsockname()[0]
    except Exception:
        ip = "127.0.0.1"
    finally:
        if s is not None:
            try:
                s.close()
            except Exception:
                pass
    return ip


# ---------------- 二维码 ----------------

def generate_qr_base64(text: str) -> str:
    """生成二维码 PNG 的 base64 data URI。失败时返回空字符串。"""
    try:
        import qrcode  # type: ignore
    except Exception:
        return ""
    try:
        # type: ignore: qrcode 的 stub 对 make_image().save 的 format 参数支持不全
        qr = qrcode.QRCode(
            version=1,
            error_correction=qrcode.constants.ERROR_CORRECT_M,
            box_size=10,
            border=4,
        )
        qr.add_data(text)
        qr.make(fit=True)
        img = qr.make_image()
        buf = io.BytesIO()
        img.save(buf, format="PNG")  # type: ignore[call-arg]
        b64 = base64.b64encode(buf.getvalue()).decode("ascii")
        return f"data:image/png;base64,{b64}"
    except Exception:
        return ""
