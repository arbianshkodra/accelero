from fastapi import FastAPI, Request

app = FastAPI()


@app.get("/")
async def home():
    return {"message": "Hello World"}


@app.post("/webhook")
async def webhook(request: Request):
    data = await request.json()
    print(data)
    return {"status": "ok"}
