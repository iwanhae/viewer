import { expect, test, type Page, type Route } from '@playwright/test'
import fs from 'node:fs/promises'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const here = path.dirname(fileURLToPath(import.meta.url))
const rootDir = path.resolve(here, '..', '..')
const samplesDir = process.env.SCREENSHOT_DIR ?? path.join(rootDir, 'samples')
const fixturesDir = path.join(rootDir, 'e2e', 'fixtures')
const zip1 = path.join(rootDir, 'e2e', 'fixtures', 'album-a.zip')
const zip2 = path.join(rootDir, 'e2e', 'fixtures', 'album-b.zip')
const zip1Base64 =
  'UEsDBBQAAAAIAA+ETVzTNyprPwAAAEQAAAAHABwAMDAxLnBuZ1VUCQADHVKPaR1Sj2l1eAsAAQToAwAABOgDAADrDPBz5+WS4mJgYOD19HAJAtKMIMzBAiS3yvAwASluTxfHkIpbyX/+yzMwMzMxvLtafhIozODp6ueyzimhCQBQSwMEFAAAAAgAD4RNXKnVyORAAAAARAAAAAcAHAAwMDIucG5nVVQJAAMdUo9pHVKPaXV4CwABBOgDAAAE6AMAAOsM8HPn5ZLiYmBg4PX0cAkC0kxAzMjBAiT/1EzJAVLcni6OIRW3kv+cP8DAyszYsPJ9ciFQmMHT1c9lnVNCEwBQSwECHgMUAAAACAAPhE1c0zcqaz8AAABEAAAABwAYAAAAAAAAAAAApIEAAAAAMDAxLnBuZ1VUBQADHVKPaXV4CwABBOgDAAAE6AMAAFBLAQIeAxQAAAAIAA+ETVyp1cjkQAAAAEQAAAAHABgAAAAAAAAAAACkgYAAAAAwMDIucG5nVVQFAAMdUo9pdXgLAAEE6AMAAAToAwAAUEsFBgAAAAACAAIAmgAAAAEBAAAAAA=='
const zip2Base64 = zip1Base64
const wallColumns = 3

type FeedItem = {
  albumId: string
  i: number
  w: number
  h: number
}

type FeedResponse = {
  items: FeedItem[]
}

async function ensureFixtureZips() {
  await fs.mkdir(fixturesDir, { recursive: true })
  await fs.writeFile(zip1, Buffer.from(zip1Base64, 'base64'))
  await fs.writeFile(zip2, Buffer.from(zip2Base64, 'base64'))
}

function distributeMasonry<T>(items: T[], columnCount: number, weight: (item: T) => number): T[][] {
  const normalizedColumnCount = Number.isFinite(columnCount) && columnCount > 0 ? Math.floor(columnCount) : 1
  const columns = Array.from({ length: normalizedColumnCount }, () => [] as T[])
  const heights = Array.from({ length: normalizedColumnCount }, () => 0)

  for (const item of items) {
    let targetColumn = 0
    let shortestHeight = heights[0]
    for (let index = 1; index < normalizedColumnCount; index++) {
      if (heights[index] < shortestHeight) {
        shortestHeight = heights[index]
        targetColumn = index
      }
    }

    columns[targetColumn].push(item)
    const value = weight(item)
    heights[targetColumn] += Number.isFinite(value) && value > 0 ? value : 1
  }

  return columns
}

function wallOrderSrcs(items: FeedItem[], columns: number): string[] {
  return distributeMasonry(items, columns, (item) => item.h / Math.max(item.w, 1))
    .flatMap((column) => column.map((item) => `/api/image/${item.albumId}/${item.i}`))
}

async function fetchFeedForSeed(page: Page, seed: string): Promise<FeedResponse> {
  const response = await page.request.get(`/api/feed?limit=80&seed=${encodeURIComponent(seed)}`)
  expect(response.ok()).toBeTruthy()
  const payload = (await response.json()) as FeedResponse
  expect(Array.isArray(payload.items)).toBeTruthy()
  return payload
}

async function collectWallSrcs(page: Page, count: number): Promise<string[]> {
  const images = page.getByTestId('wall-tile').locator('img')
  const srcs: string[] = []
  for (let i = 0; i < count; i++) {
    const src = await images.nth(i).getAttribute('src')
    expect(src).toBeTruthy()
    srcs.push(src!)
  }
  return srcs
}

async function uploadAndFinalizeFromUploadPage(page: Page, zipPath: string) {
  const zipBytes = await fs.readFile(zipPath)

  const multipartRoute = async (route: Route) => {
    const request = route.request()
    const url = request.url()
    if (!url.includes('X-Amz-Algorithm=')) {
      await route.continue()
      return
    }

    if (request.method() === 'OPTIONS') {
      await route.fulfill({
        status: 204,
        headers: {
          'access-control-allow-origin': '*',
          'access-control-allow-methods': 'PUT, OPTIONS',
          'access-control-allow-headers':
            request.headerValue('access-control-request-headers') ?? '*',
          'access-control-max-age': '86400',
        },
      })
      return
    }

    if (request.method() === 'PUT') {
      const payload = request.postDataBuffer() ?? zipBytes
      const requestHeaders = request.headers()
      delete requestHeaders.host
      delete requestHeaders['content-length']

      const response = await page.request.fetch(url, {
        method: 'PUT',
        headers: requestHeaders,
        data: payload,
      })
      await route.fulfill({
        response,
        headers: {
          ...response.headers(),
          'access-control-allow-origin': '*',
          'access-control-expose-headers': 'ETag',
        },
      })
      return
    }

    await route.continue()
  }

  await page.route('**/*', multipartRoute)
  await page.getByTestId('wall-upload').click()
  await expect.poll(() => new URL(page.url()).pathname).toBe('/upload')
  await expect(page.getByTestId('upload-page')).toBeVisible()

  try {
    await page.getByTestId('upload-pick-input').setInputFiles(zipPath)
    await expect(page.getByTestId('upload-status').first()).toHaveText(/Ready/, { timeout: 300_000 })

    await page.getByTestId('upload-back-wall').click()
    await expect.poll(() => new URL(page.url()).pathname).toBe('/')
    await expect(page.getByTestId('wall-grid')).toBeVisible()
  } finally {
    if (!page.isClosed()) {
      await page.unroute('**/*', multipartRoute)
    }
  }
}

test('basic upload -> wall -> album flow', async ({ page }) => {
  test.setTimeout(300_000)
  await fs.mkdir(samplesDir, { recursive: true })
  await ensureFixtureZips()

  await page.goto('/')
  await expect(page.getByTestId('wall-grid')).toBeVisible()
  await expect(page.getByTestId('masonry-column')).toHaveCount(3)
  await expect(page.getByTestId('wall-refresh')).toBeVisible()
  await expect(page.getByTestId('wall-find')).toBeVisible()
  await expect(page.getByTestId('wall-upload')).toBeVisible()

  await uploadAndFinalizeFromUploadPage(page, zip1)

  await expect(page.getByTestId('wall-tile').first()).toBeVisible({ timeout: 60_000 })
  const firstWallImage = page.getByTestId('wall-tile').first().locator('img')
  const wallSrc = await firstWallImage.getAttribute('src')
  expect(wallSrc).toBeTruthy()
  const wallResponse = await page.request.get(wallSrc!)
  expect(wallResponse.ok()).toBeTruthy()
  const originalResponse = await page.request.get(wallSrc!.split('?')[0])
  expect(originalResponse.ok()).toBeTruthy()
  expect(wallResponse.headers()['content-type']).toBe(originalResponse.headers()['content-type'])
  expect(await wallResponse.body()).toEqual(await originalResponse.body())

  await page.getByTestId('wall-find').click()
  await expect.poll(() => new URL(page.url()).pathname).toBe('/albums/find')
  await expect(page.getByTestId('album-search-page')).toBeVisible()
  const searchInput = page.getByTestId('album-search-input')
  await expect(searchInput).toBeVisible()
  await searchInput.fill('album-a')
  await expect(page.getByTestId('album-search-item').first()).toBeVisible({ timeout: 60_000 })
  await page.getByTestId('album-search-item').first().click()
  await expect(page.getByTestId('album-grid')).toBeVisible({ timeout: 60_000 })
  await page.getByTestId('album-back').click()
  await expect(page.getByTestId('wall-grid')).toBeVisible()

  const initialSeed = new URL(page.url()).searchParams.get('seed')
  expect(initialSeed).toBeTruthy()
  const pinnedFeedSnapshot = await fetchFeedForSeed(page, initialSeed!)
  const pinnedFeedPattern = `**/api/feed?*seed=${encodeURIComponent(initialSeed!)}*`
  await page.route(pinnedFeedPattern, async (route) => {
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify(pinnedFeedSnapshot),
    })
  })
  const expectedWallSrcs = wallOrderSrcs(pinnedFeedSnapshot.items, wallColumns)
  const sampleCount = Math.min(10, expectedWallSrcs.length)
  expect(sampleCount).toBeGreaterThan(0)
  const expectedTopSrcs = expectedWallSrcs.slice(0, sampleCount)

  try {
    await page.goto(`/?seed=${initialSeed!}`)
    await expect(page.getByTestId('wall-tile').first()).toBeVisible({ timeout: 60_000 })
    const replayWallSrcs = await collectWallSrcs(page, sampleCount)
    expect(replayWallSrcs).toEqual(expectedTopSrcs)

    await page.goto(`/?seed=${initialSeed!}`)
    await expect(page.getByTestId('wall-tile').first()).toBeVisible({ timeout: 60_000 })
    const replayAgainWallSrcs = await collectWallSrcs(page, sampleCount)
    expect(replayAgainWallSrcs).toEqual(expectedTopSrcs)
  } finally {
    await page.unroute(pinnedFeedPattern)
  }

  await page.evaluate(() => window.scrollTo(0, document.body.scrollHeight))
  const refreshButton = page.getByTestId('wall-refresh')
  const seedBeforeRefresh = new URL(page.url()).searchParams.get('seed')
  expect(seedBeforeRefresh).toBeTruthy()
  await refreshButton.click()
  await expect.poll(() => page.evaluate(() => window.scrollY)).toBe(0)
  await expect.poll(() => new URL(page.url()).searchParams.get('seed')).not.toBe(seedBeforeRefresh)
  await expect(page.getByTestId('wall-tile').first()).toBeVisible({ timeout: 60_000 })

  await page.screenshot({ path: path.join(samplesDir, '01-upload-finalize.png'), fullPage: true })

  const seedBeforeAlbumNavigation = new URL(page.url()).searchParams.get('seed')
  expect(seedBeforeAlbumNavigation).toBeTruthy()

  await page.getByTestId('wall-tile').first().click()
  await expect(page.getByTestId('album-grid')).toBeVisible()
  await expect(page.getByTestId('masonry-column')).toHaveCount(3)
  await expect(page.getByTestId('album-tile').first()).toBeVisible()
  const albumURL = new URL(page.url())
  expect(albumURL.searchParams.get('i')).toBeNull()
  const albumPath = albumURL.pathname
  const albumId = albumPath.split('/').at(-1)
  expect(albumId).toBeTruthy()

  await expect(page.getByTestId('album-columns-4')).toBeVisible()
  await expect(page.getByTestId('album-columns-6')).toBeVisible()
  await page.getByTestId('album-columns-6').click()
  await expect(page.getByTestId('masonry-column')).toHaveCount(6)
  await page.screenshot({ path: path.join(samplesDir, '02-album.png'), fullPage: true })

  await page.getByTestId('album-tile').first().click()
  await expect(page.getByTestId('photo-page')).toBeVisible()
  await expect
    .poll(() => new URL(page.url()).pathname)
    .toMatch(new RegExp(`^/album/${albumId}/\\d+$`))
  const firstOriginalLink = page.getByTestId('photo-open-original')
  await expect(firstOriginalLink).toBeVisible()
  await expect(firstOriginalLink).toHaveAttribute('target', '_blank')
  await expect(firstOriginalLink).toHaveAttribute('rel', 'noopener noreferrer')
  await expect(firstOriginalLink).toHaveAttribute('href', /^\/api\/image\/[^/]+\/\d+$/)
  expect(await firstOriginalLink.getAttribute('href')).toContain(`/api/image/${albumId}/`)
  await page.getByTestId('photo-back').click()
  await expect(page.getByTestId('album-grid')).toBeVisible()
  await expect.poll(() => new URL(page.url()).pathname).toBe(`/album/${albumId}`)

  await page.getByTestId('album-back').click()
  await expect(page.getByTestId('wall-grid')).toBeVisible()
  await expect.poll(() => new URL(page.url()).pathname).toBe('/')
  await expect.poll(() => new URL(page.url()).searchParams.get('seed')).toBe(seedBeforeAlbumNavigation)
  await expect(page.getByTestId('columns-6')).toBeVisible()
  await page.screenshot({ path: path.join(samplesDir, '03-wall.png'), fullPage: true })

  await uploadAndFinalizeFromUploadPage(page, zip2)
  await expect(page.getByTestId('wall-tile').first()).toBeVisible({ timeout: 60_000 })
  await page.screenshot({ path: path.join(samplesDir, '04-second-album.png'), fullPage: true })
})
