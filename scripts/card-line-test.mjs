import assert from 'node:assert/strict'
import fs from 'node:fs/promises'
import ts from '../frontend/node_modules/typescript/lib/typescript.js'

const source=await fs.readFile(new URL('../frontend/src/card-line.ts',import.meta.url),'utf8')
const {outputText}=ts.transpileModule(source,{compilerOptions:{module:ts.ModuleKind.ESNext,target:ts.ScriptTarget.ES2022}})
const {parseCardLine}=await import('data:text/javascript;base64,'+Buffer.from(outputText).toString('base64'))
const now=new Date(2026,9,28)
const valid=['4242424242424242----12/35----123','4242 4242 4242 4242----12/2035----012','4242-4242-4242-4242|12|2035|123','4242424242424242｜12｜35｜123','4242424242424242\t12\t2035\t123','378282246310005----10/26----1234']
for(const raw of valid){const r=parseCardLine(raw,now);assert.equal(r.error,'');assert.ok(r.card);assert.match(r.card.number,/^\d{12,19}$/);assert.match(r.card.year,/^20\d{2}$/)}
const invalid=['','admin','4242424242424242','4242424242424241----12/35----123','4242424242424242----00/35----123','4242424242424242----13/35----123','4242424242424242----09/26----123','4242424242424242----10/2025----123','4242424242424242----10/203----123','4242424242424242----10/2052----123','4242424242424242----10/35----1','4242424242424242----10/35----abcd','0000000000000000----12/35----123','4242424242424242|12|2035|123|extra']
for(const raw of invalid){const r=parseCardLine(raw,now);assert.equal(r.card,null);assert.equal(r.error.includes('424242'),false)}
console.log(`Card line validation: ${valid.length+invalid.length} cases passed`)
