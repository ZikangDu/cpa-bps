#!/usr/bin/env python3
"""Create only synthetic document fixtures; no user data is consumed."""
from pathlib import Path
from zipfile import ZipFile, ZIP_DEFLATED
import hashlib
import io
import json
from PIL import Image, ImageDraw, ImageFont
from pypdf import PdfWriter

ROOT=Path(__file__).resolve().parent

def pdf(objects):
    out=bytearray(b'%PDF-1.4\n%synthetic\n');offsets=[0]
    for index,obj in enumerate(objects,1):
        offsets.append(len(out));out.extend(f'{index} 0 obj\n'.encode()+obj+b'\nendobj\n')
    xref=len(out);out.extend(f'xref\n0 {len(offsets)}\n0000000000 65535 f \n'.encode())
    for offset in offsets[1:]:out.extend(f'{offset:010d} 00000 n \n'.encode())
    out.extend(f'trailer\n<< /Size {len(offsets)} /Root 1 0 R >>\nstartxref\n{xref}\n%%EOF\n'.encode());return bytes(out)

def page_pdf(content):
    return pdf([b'<< /Type /Catalog /Pages 2 0 R >>',b'<< /Type /Pages /Count 1 /Kids [3 0 R] >>',
      b'<< /Type /Page /Parent 2 0 R /MediaBox [0 0 400 400] /Resources << /Font << /F1 5 0 R >> >> /Contents 4 0 R >>',
      f'<< /Length {len(content)} >>\nstream\n'.encode()+content+b'endstream',b'<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>'])

def main():
    parts={
     '[Content_Types].xml':'<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/><Override PartName="/xl/worksheets/sheet1.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/></Types>',
     '_rels/.rels':'<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/></Relationships>',
     'xl/workbook.xml':'<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets><sheet name="Financials" sheetId="1" r:id="rId1"/></sheets></workbook>',
     'xl/_rels/workbook.xml.rels':'<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/></Relationships>',
     'xl/worksheets/sheet1.xml':'<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData><row r="1"><c r="A1" t="inlineStr"><is><t>Metric</t></is></c><c r="B1" t="inlineStr"><is><t>Value</t></is></c></row><row r="2"><c r="A2" t="inlineStr"><is><t>Sales</t></is></c><c r="B2"><v>120</v></c></row><row r="3"><c r="A3" t="inlineStr"><is><t>Costs</t></is></c><c r="B3"><v>80</v></c></row><row r="4"><c r="A4" t="inlineStr"><is><t>Profit</t></is></c><c r="B4"><f>B2-B3</f><v>40</v></c></row><row r="5"><c r="A5" t="inlineStr"><is><t>DOCUMENT_XLSX_FIXTURE_056</t></is></c></row></sheetData></worksheet>'}
    with ZipFile(ROOT/'financials.xlsx','w',ZIP_DEFLATED) as z:
        for name,data in parts.items():z.writestr(name,data)
    (ROOT/'financials.csv').write_text('Metric,Value\nSales,120\nCosts,80\nProfit,40\nDOCUMENT_CSV_FIXTURE_056,1\n')
    text=b'BT /F1 16 Tf 30 350 Td (Financial report) Tj 0 -35 Td (Sales: 120) Tj 0 -35 Td (Costs: 80) Tj 0 -35 Td (Profit: 40) Tj 0 -35 Td (DOCUMENT_PDF_FIXTURE_056) Tj ET\n'
    (ROOT/'financials.pdf').write_bytes(page_pdf(text))
    chart=b'BT /F1 20 Tf 30 360 Td (Quarterly units) Tj ET\n0.2 0.4 0.8 rg 70 60 70 100 re f 220 60 70 200 re f\n0 0 0 rg BT /F1 18 Tf 90 35 Td (A) Tj 150 0 Td (B) Tj ET\nBT /F1 18 Tf 85 175 Td (10) Tj 150 100 Td (20) Tj ET\nBT /F1 10 Tf 20 395 Td (DOCUMENT_CHART_FIXTURE_056) Tj ET\n'
    (ROOT/'mixed-chart.pdf').write_bytes(page_pdf(chart))
    image=Image.new('RGB',(1200,1600),'white');draw=ImageDraw.Draw(image)
    font_path=next((str(p) for p in [Path('/System/Library/Fonts/Supplemental/Arial.ttf'),Path('/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf')] if p.exists()),None)
    heading=ImageFont.truetype(font_path,64) if font_path else ImageFont.load_default()
    font=ImageFont.truetype(font_path,76) if font_path else ImageFont.load_default()
    draw.text((90,130),'Financial report',fill='black',font=heading)
    for y,label in [(380,'Sales: 120'),(620,'Costs: 80'),(860,'Profit: 40')]:draw.text((90,y),label,fill='black',font=font)
    draw.line((90,1050,1110,1050),fill='black',width=4)
    image.save(ROOT/'scan-financials.png');image.save(ROOT/'scan-financials.pdf','PDF',resolution=150.0)
    writer=PdfWriter();writer.append(str(ROOT/'financials.pdf'));writer.encrypt('fixture-password')
    with (ROOT/'encrypted.pdf').open('wb') as f:writer.write(f)
    (ROOT/'corrupt.pdf').write_bytes(b'%PDF-1.7\nnot a document\n%%EOF\n')
    manifest={'synthetic_only':True,'files':{p.name:{'bytes':p.stat().st_size,'sha256':hashlib.sha256(p.read_bytes()).hexdigest()} for p in ROOT.iterdir() if p.suffix in ('.xlsx','.csv','.pdf','.png')}}
    (ROOT/'manifest.json').write_text(json.dumps(manifest,indent=2)+'\n')

if __name__=='__main__':main()
