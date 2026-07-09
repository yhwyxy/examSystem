from backend.main import app

if __name__ == "__main__":
    import uvicorn

    from backend.config import get_config

    cfg = get_config().server
    uvicorn.run("backend.main:app", host=cfg.host, port=cfg.port, reload=True)
