const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const http = require('node:http')
const crypto = require('node:crypto')
const {execFileSync} = require('node:child_process')
const {pathToFileURL} = require('node:url')

const plugin = path.resolve(__dirname, '..')
const output = path.resolve('.omc/tmp/pixel-town-parity')
const approved = path.join(__dirname, 'recorded/pixel-town-approved.html')
assert.equal(crypto.createHash('sha256').update(fs.readFileSync(approved)).digest('hex'), 'bcefe437e0f396e2fd5138efd47dc2fca155ebf47aa1bb2eb9a08ad0601319f4')
const playwright = require(process.env.PLAYWRIGHT_PATH || path.join(execFileSync('npm', ['root', '-g'], {encoding:'utf8'}).trim(), 'playwright'))
const chrome = process.env.CHROME_PATH || ['/usr/bin/google-chrome', '/usr/bin/chromium'].find(filename => fs.existsSync(filename))
assert.ok(chrome, 'Chrome must be installed; parity cannot be skipped')

async function main() {
  fs.mkdirSync(output, {recursive:true})
  const {projectSeeds,ticketSeeds}=await import(pathToFileURL(path.join(__dirname,'design-preview/seeds.mjs')))
  const projectRows=Array.from({length:8},(_,index)=>projectSeeds[index]??{id:`project-${index+1}`,name:`小镇工坊 ${String(index+1).padStart(4,'0')}`})
  const scene={generatedAt:1793970000000,state:'ok',maintenance:'running',rows:projectRows.map(project=>({name:project.name,path:'/fixture/'+project.id,workers:1,deploy:true,parcels:ticketSeeds.filter(ticket=>ticket.project===project.id).map(ticket=>({identifier:ticket.id,title:ticket.title,station:ticket.stage,phase:ticket.stage,label:ticket.stage,duration:`${ticket.minutes} 分钟`,tone:ticket.tone,reason:ticket.reason,unknownDeploy:false}))}))}
  const streams=new Set()
  const server = http.createServer((request, response) => {
    const url=new URL(request.url,'http://localhost')
    if(url.pathname==='/snapshot'){response.setHeader('Content-Type','application/json');response.end(JSON.stringify(scene));return}
    if(url.pathname==='/events'){response.writeHead(200,{'Content-Type':'text/event-stream'});response.write(`event: scene\ndata: ${JSON.stringify(scene)}\n\n`);streams.add(response);request.on('close',()=>streams.delete(response));return}
    const production=url.pathname==='/'||url.pathname.startsWith('/components/')||['/scene.js','/town.mjs'].includes(url.pathname)
    const relative=production?'/scene/page/'+(url.pathname==='/'?'index.html':url.pathname.slice(1)):url.pathname
    const filename = path.resolve(plugin, '.' + relative)
    if (!filename.startsWith(plugin + path.sep) || !fs.existsSync(filename) || !fs.statSync(filename).isFile()) {response.writeHead(404).end(); return}
    response.setHeader('Content-Type', filename.endsWith('.html') ? 'text/html' : filename.endsWith('.css') ? 'text/css' : 'text/javascript')
    response.end(fs.readFileSync(filename,'utf8').replaceAll('__TOKEN__','fixture-token'))
  })
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve))
  let browser
  try {
    browser = await playwright.chromium.launch({executablePath:chrome, headless:true, args:['--no-sandbox']})
    const origin = `http://127.0.0.1:${server.address().port}`
    const results = []
    for (const viewport of [{width:1440,height:1000},{width:1920,height:1080},{width:390,height:844}]) {
      for (const view of ['project','town']) {
        const context = await browser.newContext({viewport, deviceScaleFactor:1, reducedMotion:'reduce'})
        const errors = []
        const pages = await Promise.all([context.newPage(),context.newPage(),context.newPage()])
        for(const page of pages) {
          page.on('pageerror', error => errors.push(error.message))
          await page.addInitScript(() => { Date.now = () => 1793970000000; performance.now = () => 4000 })
        }
        const query = `?count=8&view=${view}&project=orca`
        await pages[0].goto(origin+'/test/recorded/pixel-town-approved.html'+query)
        await pages[1].goto(origin+'/test/design-preview/index.html'+query)
        await pages[2].goto(origin+`/?t=fixture-token&view=${view}&project=${encodeURIComponent('/fixture/orca')}`)
        for(const page of pages) {
          await page.waitForFunction(() => document.querySelectorAll('.repo-button').length === 8)
          await page.evaluate(() => document.fonts.ready)
          await page.waitForTimeout(250)
        }
        assert.deepEqual(errors, [], 'both pages must render without exceptions')
        const prefix = `${viewport.width}-${viewport.height}-${view}`
        const original = await pages[0].screenshot({path:path.join(output,prefix+'-original.png')})
        const repeat = await pages[0].screenshot({path:path.join(output,prefix+'-repeat.png')})
        assert.ok(original.equals(repeat),`${prefix}: approved source must be stable before comparison; see ${prefix}-original.png and ${prefix}-repeat.png`)
        const candidate = await pages[1].screenshot({path:path.join(output,prefix+'-candidate.png')})
        await pages[2].screenshot({path:path.join(output,prefix+'-production.png')})
        const sourceCanvas=await pages[0].locator('#world').screenshot()
        const productionCanvas=await pages[2].locator('#world').screenshot()
        fs.writeFileSync(path.join(output,prefix+'-source-canvas.png'),sourceCanvas)
        fs.writeFileSync(path.join(output,prefix+'-production-canvas.png'),productionCanvas)
        assert.ok(sourceCanvas.equals(productionCanvas),`${prefix}: production must use identical room, devices, actors and belts; see ${prefix}-source-canvas.png and ${prefix}-production-canvas.png`)
        if(view==='project')assert.equal(await pages[2].locator('[data-station]').count(),11)
        assert.equal(await pages[2].locator('#advance').count(),0)
        assert.equal(await pages[2].locator('#project-count').isVisible(),false)
        for(const selector of ['.topbar','.navigation','.project-sidebar','.map-column','.detail-panel']){
          assert.deepEqual(await pages[2].locator(selector).boundingBox(),await pages[0].locator(selector).boundingBox(),`${selector} must keep the approved layout`)
        }
        const diff = await pages[1].evaluate(async ({original,candidate}) => {
          const decode = async encoded => createImageBitmap(await (await fetch('data:image/png;base64,'+encoded)).blob())
          const images = await Promise.all([decode(original), decode(candidate)])
          const canvases = images.map(image => {const canvas=document.createElement('canvas');canvas.width=image.width;canvas.height=image.height;canvas.getContext('2d').drawImage(image,0,0);return canvas})
          const pixels = canvases.map(canvas=>canvas.getContext('2d').getImageData(0,0,canvas.width,canvas.height))
          let count=0
          for(let offset=0;offset<pixels[0].data.length;offset+=4){
            const changed=[0,1,2,3].some(channel=>pixels[0].data[offset+channel]!==pixels[1].data[offset+channel])
            if(changed)count++
            pixels[0].data.set(changed?[255,0,80,255]:[255,255,255,255],offset)
          }
          canvases[0].getContext('2d').putImageData(pixels[0],0,0)
          return {count,png:canvases[0].toDataURL().split(',')[1]}
        },{original:original.toString('base64'),candidate:candidate.toString('base64')})
        fs.writeFileSync(path.join(output,prefix+'-diff.png'),Buffer.from(diff.png,'base64'))
        results.push({viewport,view,differentPixels:diff.count})
        console.log(`${prefix}: ${diff.count} different pixels`)
        await context.close()
      }
    }
    fs.writeFileSync(path.join(output,'results.json'),JSON.stringify(results,null,2))
    assert.equal(results.length,6,'all viewport/view combinations must execute')
    assert.ok(results.every(result=>result.differentPixels===0),'every frame must have zero different pixels; inspect original/candidate/diff evidence')
    console.log('PASS 6 stable approved-source comparisons with zero different pixels and 6 production canvas comparisons')
  } finally {await browser?.close();for(const stream of streams)stream.end();await new Promise(resolve => server.close(resolve))}
}
main().catch(error=>{console.error(error);process.exitCode=1})
