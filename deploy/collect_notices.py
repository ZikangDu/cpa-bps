#!/usr/bin/env python3
"""Collect module LICENSE/NOTICE text from an offline-populated Go module cache."""
import json,pathlib,shutil,subprocess,sys
root=pathlib.Path(sys.argv[1]);root.mkdir(parents=True,exist_ok=True)
raw=subprocess.check_output(['go','list','-m','-json','all'],text=True)
decoder=json.JSONDecoder();index=0;manifest=[]
while index<len(raw):
 while index<len(raw) and raw[index].isspace():index+=1
 if index>=len(raw):break
 item,index=decoder.raw_decode(raw,index)
 actual=item.get('Replace') or item
 if item.get('Main') or not actual.get('Dir'):continue
 source=pathlib.Path(actual['Dir']);target=root/(item['Path'].replace('/','_')+'@'+actual.get('Version','unknown'))
 target.mkdir(exist_ok=True);copied=[]
 for entry in source.iterdir():
  if entry.is_file() and entry.name.upper().split('.')[0] in ('LICENSE','LICENCE','NOTICE','COPYING','COPYRIGHT'):
   shutil.copy2(entry,target/entry.name);copied.append(entry.name)
 manifest.append({'module':item['Path'],'version':actual.get('Version'),'files':copied})
(root/'manifest.json').write_text(json.dumps(manifest,indent=2)+'\n')
