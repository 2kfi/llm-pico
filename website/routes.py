from fastapi import APIRouter
from fastapi.responses import FileResponse, HTMLResponse, RedirectResponse
from pathlib import Path

router = APIRouter()
STATIC_DIR = Path(__file__).parent / "static"

_MIME = {
    ".css": "text/css",
    ".js": "application/javascript",
    ".json": "application/json",
    ".svg": "image/svg+xml",
    ".png": "image/png",
    ".jpg": "image/jpeg",
    ".ico": "image/x-icon",
    ".woff2": "font/woff2",
    ".woff": "font/woff",
    ".ttf": "font/ttf",
}


def _safe_path(sub: str) -> Path | None:
    """Resolve sub-path under STATIC_DIR, reject traversal."""
    full = (STATIC_DIR / sub).resolve()
    if not str(full).startswith(str(STATIC_DIR.resolve())):
        return None
    return full


@router.get("/dashboard")
async def dashboard_redirect():
    return RedirectResponse(url="/admin/dashboard/", status_code=307)


@router.get("/dashboard/")
async def dashboard_index():
    return FileResponse(STATIC_DIR / "index.html")


@router.get("/dashboard/{path:path}")
async def dashboard_catchall(path: str):
    safe = _safe_path(path)
    if safe and safe.is_file():
        media_type = _MIME.get(safe.suffix)
        return FileResponse(safe, media_type=media_type)
    return HTMLResponse((STATIC_DIR / "index.html").read_text())
