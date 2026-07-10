from fastapi import APIRouter
from starlette.responses import JSONResponse

router = APIRouter()

NOT_IMPLEMENTED = {
    "images": {
        "generations": "Image generation is not yet supported",
        "edits": "Image editing is not yet supported",
        "variations": "Image variations are not yet supported",
    },
    "audio": {
        "transcriptions": "Audio transcription is not yet supported",
        "translations": "Audio translation is not yet supported",
        "speech": "Text-to-speech is not yet supported",
    },
    "moderations": {"moderations": "Moderation is not yet supported"},
}


def _make_placeholder(group: str, feature: str) -> callable:
    async def handler():
        return JSONResponse(
            status_code=501,
            content={
                "error": {
                    "message": NOT_IMPLEMENTED[group][feature],
                    "type": "not_implemented",
                    "code": 501,
                }
            },
        )
    return handler


for group, features in NOT_IMPLEMENTED.items():
    for feature in features:
        if group == "moderations":
            router.add_api_route(
                f"/v1/moderations",
                _make_placeholder(group, feature),
                methods=["POST"],
                include_in_schema=False,
            )
        else:
            router.add_api_route(
                f"/v1/{group}/{feature}",
                _make_placeholder(group, feature),
                methods=["POST"],
                include_in_schema=False,
            )
