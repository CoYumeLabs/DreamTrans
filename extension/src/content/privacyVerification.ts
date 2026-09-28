import { classifyModule } from './discovery'
import { fetchModuleFiles } from './fetcher'
import { moodleFetch, RateLimiter } from './limits'
import type { MoodleContext } from '../shared/types'

for (const modtype of ['forum', 'hsuforum', 'forumng']) for (const name of ['Discussion', 'News', 'Announcements', '公告讨论', 'notice board']) {
  const module = { cmid: 1, modtype, name, contents: [] }
  if (classifyModule(module).skipped !== 'private') throw new Error(`Forum not excluded: ${modtype} ${name}`)
  // The fetch layer also refuses an unclassified forum: callers cannot bypass
  // the privacy boundary by forgetting discovery or renaming a discussion.
  const files = await fetchModuleFiles(new RateLimiter(), { wwwroot: 'https://moodle.example.test' } as MoodleContext, { ...module, skipped: undefined })
  if (files.length) throw new Error(`Forum fetched: ${name}`)
}
console.log('All forums excluded at discovery and fetch boundaries.')

// Redirects: only a file download may leave the Moodle host, only to https,
// and only when what comes back is not a page.
;(globalThis as { location?: unknown }).location = { href: 'https://moodle.example.test/course/view.php?id=1' }
const realFetch = globalThis.fetch
function answer(finalUrl: string, contentType: string, status = 200): void {
  globalThis.fetch = (async () => {
    const response = new Response('x', { status, headers: { 'content-type': contentType } })
    Object.defineProperty(response, 'url', { value: finalUrl })
    return response
  }) as typeof fetch
}
async function refused(run: () => Promise<unknown>): Promise<boolean> {
  try { await run(); return false } catch { return true }
}
const start = 'https://moodle.example.test/pluginfile.php/1/a.pdf'
const cases: Array<[string, string, string, boolean, boolean]> = [
  ['CDN file', 'https://cdn.example.net/a?sig=1', 'application/pdf', true, false],
  ['CDN file without opt-in', 'https://cdn.example.net/a?sig=1', 'application/pdf', false, true],
  ['off-site page', 'https://sso.example.net/login', 'text/html; charset=utf-8', true, true],
  ['plain http CDN', 'http://cdn.example.net/a', 'application/pdf', true, true],
  ['same-host page', 'https://moodle.example.test/mod/page/view.php?id=2', 'text/html', false, false],
]
for (const [label, finalUrl, contentType, allow, shouldRefuse] of cases) {
  answer(finalUrl, contentType)
  const wasRefused = await refused(() => moodleFetch(new RateLimiter(1, 0), start, undefined, { allowFileRedirect: allow }))
  if (wasRefused !== shouldRefuse) throw new Error(`Redirect policy wrong for ${label}`)
}
globalThis.fetch = realFetch
console.log('Redirects leave Moodle only for https file downloads.')
