"""Bounded, local-only PDF extraction for the CPA BPS document worker.

Runtime dependency: pypdfium2==5.12.1 (bundled standard, non-V8 PDFium).
No PDF JavaScript/form environment, attachment, URL, action or link is executed.
"""
import base64
from contextlib import closing
import json
import math
import os
import struct
import sys
import zlib


def emit(value):
    sys.stdout.buffer.write(json.dumps(value, ensure_ascii=False, separators=(",", ":")).encode("utf-8"))
    sys.stdout.buffer.flush()


def fail(code):
    emit({"ok": False, "error": code})
    return 1


def bounded_png(bitmap, maximum):
    # PNG encoding uses stdlib only; no optional Pillow dependency is required.
    # The page bitmap is bounded before render, so raw scanlines remain bounded.
    width, height = bitmap.width, bitmap.height
    pixels = bytes(bitmap.buffer)
    mode = bitmap.mode
    raw = bytearray()
    for y in range(height):
        raw.append(0)
        row = pixels[y * bitmap.stride : (y + 1) * bitmap.stride]
        rgb = bytearray(width * 3)
        if mode in ("BGR", "BGRx", "BGRA"):
            step = 3 if mode == "BGR" else 4
            rgb[0::3] = row[2:width * step:step]
            rgb[1::3] = row[1:width * step:step]
            rgb[2::3] = row[0:width * step:step]
        elif mode in ("RGB", "RGBx", "RGBA"):
            step = 3 if mode == "RGB" else 4
            rgb[0::3] = row[0:width * step:step]
            rgb[1::3] = row[1:width * step:step]
            rgb[2::3] = row[2:width * step:step]
        elif mode == "L":
            rgb[0::3] = row[:width]
            rgb[1::3] = row[:width]
            rgb[2::3] = row[:width]
        else:
            raise ValueError("unsupported_bitmap")
        raw.extend(rgb)
    def chunk(kind, data):
        return struct.pack(">I", len(data)) + kind + data + struct.pack(">I", zlib.crc32(kind + data) & 0xFFFFFFFF)
    compressed = zlib.compress(bytes(raw), 6)
    encoded = (b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", struct.pack(">IIBBBBB", width, height, 8, 2, 0, 0, 0))
               + chunk(b"IDAT", compressed) + chunk(b"IEND", b""))
    if len(encoded) > maximum:
        return None
    return encoded


def main():
    try:
        options = json.loads(sys.stdin.buffer.read(4097))
        if len(sys.argv) != 2 or not isinstance(options, dict):
            return fail("invalid_request")
        required = ("max_bytes", "max_pages", "max_text_chars", "max_visual_pages", "max_image_bytes", "max_pixels", "max_page_edge")
        if any(type(options.get(k)) is not int or options[k] <= 0 for k in required):
            return fail("invalid_request")
        if os.stat(sys.argv[1]).st_size > options["max_bytes"]:
            return fail("too_large")
        # The Go parent enforces wall time. On Linux also bound native parser
        # address space, CPU and accidental output files. No child is spawned.
        if sys.platform.startswith("linux"):
            import resource
            resource.setrlimit(resource.RLIMIT_AS, (2 << 30, 2 << 30))
            resource.setrlimit(resource.RLIMIT_CPU, (30, 31))
            resource.setrlimit(resource.RLIMIT_FSIZE, (32 << 20, 32 << 20))
        try:
            import pypdfium2 as pdfium
        except (ImportError, OSError):
            return fail("dependency_unavailable")
        try:
            document = pdfium.PdfDocument(sys.argv[1])
        except pdfium.PdfiumError as error:
            return fail("encrypted" if getattr(error, "err_code", 0) == 4 else "invalid_pdf")
        with document:
            if pdfium.raw.FPDF_GetSecurityHandlerRevision(document) >= 0:
                return fail("encrypted")
            count = len(document)
            if count <= 0:
                return fail("invalid_pdf")
            result = {"ok": True, "total_pages": count, "pages": [], "warnings": [], "truncated": False}
            def warn(message):
                result["truncated"] = True
                if message not in result["warnings"]:
                    result["warnings"].append(message)
            if count > options["max_pages"]:
                warn("pdf_page_limit: processed first %d of %d pages" % (options["max_pages"], count))
            remaining_text = options["max_text_chars"]
            remaining_images = options["max_visual_pages"]
            remaining_bytes = options["max_image_bytes"]
            remaining_pixels = options["max_pixels"]
            visual_omitted = []
            for index in range(min(count, options["max_pages"])):
                with closing(document[index]) as page:
                    with closing(page.get_textpage()) as textpage:
                        char_count = textpage.count_chars()
                        # Bound extraction before allocating a Python string.
                        text = textpage.get_text_range(0, min(char_count, remaining_text)) if remaining_text else ""
                        text = text.replace("\r\n", "\n").replace("\r", "\n")
                        text = text[:remaining_text]
                        if char_count > remaining_text:
                            warn("pdf_text_limit: text extraction was limited at page %d" % (index + 1))
                        remaining_text -= len(text)
                    graphics = False
                    try:
                        for item in page.get_objects(max_depth=15):
                            if item.type in (pdfium.raw.FPDF_PAGEOBJ_PATH, pdfium.raw.FPDF_PAGEOBJ_IMAGE,
                                             pdfium.raw.FPDF_PAGEOBJ_SHADING, pdfium.raw.FPDF_PAGEOBJ_FORM):
                                graphics = True
                                break
                        graphics = graphics or pdfium.raw.FPDFPage_GetAnnotCount(page) > 0
                    except Exception:
                        graphics = True
                    entry = {"number": index + 1, "text": text, "has_graphics": graphics}
                    # A page with text AND charts must still be rendered. A
                    # text budget exhaustion alone must not invent a scan.
                    needs_visual = graphics or (char_count == 0 or char_count <= len(text) and not text.strip())
                    if needs_visual:
                        if remaining_images <= 0 or remaining_bytes <= 0 or remaining_pixels <= 0:
                            visual_omitted.append(index + 1)
                        else:
                            width, height = page.get_size()
                            if not (math.isfinite(width) and math.isfinite(height) and 0 < width <= 200000 and 0 < height <= 200000):
                                return fail("invalid_pdf")
                            scale = min(options["max_page_edge"] / max(width, height), 2.0)
                            target_width = max(1, math.ceil(width * scale))
                            target_height = max(1, math.ceil(height * scale))
                            if target_width * target_height > remaining_pixels:
                                visual_omitted.append(index + 1)
                            else:
                                with closing(page.render(scale=scale, may_draw_forms=False, fill_color=(255,255,255,255), limit_image_cache=True)) as bitmap:
                                    pixels = bitmap.width * bitmap.height
                                    if pixels > remaining_pixels or max(bitmap.width, bitmap.height) > options["max_page_edge"] + 1:
                                        visual_omitted.append(index + 1)
                                    else:
                                        image = bounded_png(bitmap, min(5 << 20, remaining_bytes))
                                        if image is None:
                                            visual_omitted.append(index + 1)
                                        else:
                                            entry["image_png"] = base64.b64encode(image).decode("ascii")
                                            remaining_images -= 1
                                            remaining_bytes -= len(image)
                                            remaining_pixels -= pixels
                    result["pages"].append(entry)
            if visual_omitted:
                warn("pdf_visual_limit: visual content omitted on pages " + ",".join(map(str, visual_omitted)))
            emit(result)
            return 0
    except (ValueError, KeyError, TypeError, OSError, MemoryError):
        return fail("invalid_pdf")
    except Exception:
        # Never copy document text, filenames, paths or PDF parser diagnostics.
        return fail("invalid_pdf")


if __name__ == "__main__":
    raise SystemExit(main())
