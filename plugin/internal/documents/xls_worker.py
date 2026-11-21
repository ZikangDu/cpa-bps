"""Read legacy BIFF spreadsheet values; never execute macros or recalculate."""
import io,json,sys,math

def emit(value):
    sys.stdout.write(json.dumps(value,ensure_ascii=False,separators=(',',':')))

def address(row,col):
    value='';col+=1
    while col:
        col,rem=divmod(col-1,26);value=chr(65+rem)+value
    return value+str(row+1)

def main():
    if sys.platform.startswith('linux'):
        import resource
        resource.setrlimit(resource.RLIMIT_AS,(512<<20,512<<20))
    try: import xlrd
    except ImportError: emit({'error':'document_parser_unavailable'});return
    try:
        with open(sys.argv[1],'rb') as f: data=f.read((20<<20)+1)
        if not data or len(data)>20<<20:emit({'error':'document_size_limit'});return
        book=xlrd.open_workbook(file_contents=data,logfile=io.StringIO(),on_demand=True,ragged_rows=True,formatting_info=True)
        out={'format':'xls','sheets':[],'total_sheets':book.nsheets,'parsed_cells':0,'truncated':False,'warnings':['Legacy XLS provides stored cell values; formula expressions are unavailable and no formulas or macros were executed.']}
        budget=0
        try:
            for si in range(min(book.nsheets,32)):
                sh=book.sheet_by_index(si)
                sheet={'name':sh.name[:160],'visibility':('visible','hidden','veryHidden')[min(getattr(sh,'visibility',0),2)],'cells':[],'merged_ranges':[],'truncated':False}
                for rlo,rhi,clo,chi in sh.merged_cells[:1024]:sheet['merged_ranges'].append(address(rlo,clo)+':'+address(rhi-1,chi-1))
                for row in range(min(sh.nrows,65536)):
                    for col,cell in enumerate(sh.row(row)):
                        if cell.ctype in (xlrd.XL_CELL_EMPTY,xlrd.XL_CELL_BLANK):continue
                        value=cell.value
                        if isinstance(value,float) and not math.isfinite(value): value='[non-finite number]'
                        raw=str(value)
                        if cell.ctype==xlrd.XL_CELL_DATE:
                            try:value=xlrd.xldate_as_datetime(value,book.datemode).isoformat()
                            except (ValueError,OverflowError):value=raw
                        elif cell.ctype==xlrd.XL_CELL_BOOLEAN:value='TRUE' if value else 'FALSE'
                        elif cell.ctype==xlrd.XL_CELL_ERROR:value=xlrd.error_text_from_code.get(value,'#ERROR')
                        value=str(value)
                        if len(value.encode())>8192:value=value.encode()[:8192].decode(errors='ignore')+' [cell truncated]';out['truncated']=True
                        item={'address':address(row,col),'display_value':value,'raw_value':raw[:8192],'cached_value_present':True,'type':str(cell.ctype)}
                        cost=len(json.dumps(item,ensure_ascii=False).encode())
                        if out['parsed_cells']>=20000 or budget+cost>480<<10:
                            out['truncated']=True;sheet['truncated']=True;break
                        sheet['cells'].append(item);out['parsed_cells']+=1;budget+=cost
                    if sheet['truncated']:break
                out['sheets'].append(sheet);book.unload_sheet(si)
                if sheet['truncated']:break
            if len(out['sheets'])<book.nsheets:out['truncated']=True
            if out['truncated']:out['warnings'].append('Document extraction reached its sheet/cell/text limits; omitted content was not analyzed.')
            emit({'spreadsheet':out})
        finally:book.release_resources()
    except Exception as exc:
        code='document_encrypted' if 'encrypt' in str(exc).lower() or 'password' in str(exc).lower() else 'document_corrupt'
        emit({'error':code})

if __name__=='__main__':main()
