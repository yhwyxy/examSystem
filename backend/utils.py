"""通用工具：时间处理、局域网 IP 识别、二维码生成、相似度计算。"""
from __future__ import annotations

import base64
import io
import math
import re
import socket
from datetime import datetime, timezone
from typing import Any


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


# ---------------- 相似度计算 ----------------

def _tokenize(text: str) -> set[str]:
    """将文本分词为小写 token 集合（中文按字符，英文按单词）。"""
    # 英文单词
    en = set(re.findall(r'[a-zA-Z]+', text.lower()))
    # 中文字符（单字）
    zh = set(re.findall(r'[\u4e00-\u9fff]', text))
    return en | zh


def keyword_similarity(a: str, b: str) -> float:
    """基于关键词重叠的相似度（Jaccard）。"""
    ta = _tokenize(a)
    tb = _tokenize(b)
    if not ta and not tb:
        return 1.0
    if not ta or not tb:
        return 0.0
    return len(ta & tb) / len(ta | tb)


def cosine_similarity(a: list[float], b: list[float]) -> float:
    """余弦相似度。"""
    dot = sum(x * y for x, y in zip(a, b))
    na = math.sqrt(sum(x * x for x in a))
    nb = math.sqrt(sum(x * x for x in b))
    if na == 0 or nb == 0:
        return 0.0
    return dot / (na * nb)


def safe_score(score: float, max_score: float) -> float:
    """将分数限制在 [0, max_score] 范围内。"""
    return max(0.0, min(float(score), float(max_score)))


def review_status_by_confidence(confidence: float, cfg: Any) -> str:
    """根据置信度和配置阈值返回复核状态。"""
    if confidence >= cfg.review.high_confidence_threshold:
        return "high_confidence"
    if confidence >= cfg.review.need_review_threshold:
        return "need_review"
    if confidence >= cfg.review.low_confidence_threshold:
        return "low_confidence"
    return "need_review"


def similarity(a: str, b: str) -> float:
    """通用相似度入口，回退到关键词相似度。"""
    return keyword_similarity(a, b)
