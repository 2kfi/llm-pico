from fastapi import APIRouter
from fastapi.responses import FileResponse
from pathlib import Path

router = APIRouter()
STATIC_DIR = Path(__file__).parent / "static"


@router.get("/dashboard")
async def dashboard():
    return FileResponse(STATIC_DIR / "index.html")


@router.get("/dashboard/{path:path}")
async def dashboard_static(path: str):
    full_path = (STATIC_DIR / path).resolve()
    if not str(full_path).startswith(str(STATIC_DIR.resolve())):
        from fastapi.responses import PlainTextResponse
        return PlainTextResponse("Not found", status_code=404)
    if full_path.is_file():
        return FileResponse(full_path)
    return FileResponse(STATIC_DIR / "index.html")
