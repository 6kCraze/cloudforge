"""Regenerate original CC0 demo artwork (authoring only; requires Pillow)."""
from pathlib import Path
from PIL import Image, ImageDraw

ROOT = Path(__file__).parent


def landscape(name, sky, sun, distant, near, water):
    image = Image.new("RGB", (640, 400), sky)
    draw = ImageDraw.Draw(image)
    draw.ellipse((445, 45, 535, 135), fill=sun)
    draw.polygon([(0, 270), (150, 95), (335, 270), (480, 155), (640, 275)], fill=distant)
    draw.polygon([(0, 300), (90, 215), (240, 315), (360, 205), (550, 310), (640, 260), (640, 400), (0, 400)], fill=near)
    draw.rectangle((0, 325, 640, 400), fill=water)
    for x, y, length in [(40, 345, 150), (365, 365, 200), (215, 385, 110)]:
        draw.line((x, y, x + length, y), fill=sun, width=3)
    image.save(ROOT / name, optimize=True)


landscape("alpine-lake.png", "#d9eee9", "#fff4c7", "#83a6ad", "#284c5b", "#5d9fa8")
landscape("desert-dusk.png", "#f8dbc1", "#fff3c8", "#cb8b79", "#884d62", "#b06764")

image = Image.new("RGBA", (640, 400), (0, 0, 0, 0))
draw = ImageDraw.Draw(image)
draw.rounded_rectangle((45, 35, 595, 365), radius=40, fill="#182f43")
draw.ellipse((270, 75, 500, 305), fill="#59cbb8")
draw.polygon([(110, 300), (260, 80), (410, 300)], fill="#efc077")
draw.ellipse((170, 210, 265, 305), fill="#f8e7c9")
image.save(ROOT / "orbit-study.png", optimize=True)
