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
    file = STATIC_DIR / path
    if file.is_file():
        return FileResponse(file)
    return FileResponse(STATIC_DIR / "index.html")
