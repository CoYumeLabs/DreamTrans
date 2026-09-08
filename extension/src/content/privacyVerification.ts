import { classifyModule } from './discovery'
import { fetchModuleFiles } from './fetcher'
import { RateLimiter } from './limits'
import type { MoodleContext } from '../shared/types'

for (const name of ['Discussion', 'News', 'Announcements', '公告讨论', 'notice board']) {
  const module = { cmid: 1, modtype: 'forum', name, contents: [] }
  if (classifyModule(module).skipped !== 'private') throw new Error(`Forum not excluded: ${name}`)
  // The fetch layer also refuses an unclassified forum: callers cannot bypass
  // the privacy boundary by forgetting discovery or renaming a discussion.
  const files = await fetchModuleFiles(new RateLimiter(), { wwwroot: 'https://moodle.example.test' } as MoodleContext, { ...module, skipped: undefined })
  if (files.length) throw new Error(`Forum fetched: ${name}`)
}
console.log('All forums excluded at discovery and fetch boundaries.')
