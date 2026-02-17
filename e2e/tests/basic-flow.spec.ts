import { expect, test, type Page } from '@playwright/test'
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
const zip2Base64 =
  'UEsDBBQAAAAIAA+ETVxw8BtLQAAAAEQAAAAHABwAMDAxLnBuZ1VUCQADHVKPaR1Sj2l1eAsAAQToAwAABOgDAADrDPBz5+WS4mJgYOD19HAJAtKMQMzEwQIkXTP+igEpbk8Xx5CKW8l//s9nYGZmYnjP5jgFKMzg6ernss4poQkAUEsDBBQAAAAIAA+ETVynezQxPgAAAEQAAAAHABwAMDAyLnBuZ1VUCQADHVKPaR1Sj2l1eAsAAQToAwAABOgDAADrDPBz5+WS4mJgYOD19HAJAtJMIMzBAiT/1EzJAVLcni6OIRW3kv+cB0owMzL+V43OAgozeLr6uaxzSmgCAFBLAQIeAxQAAAAIAA+ETVxw8BtLQAAAAEQAAAAHABgAAAAAAAAAAACkgQAAAAAwMDEucG5nVVQFAAMdUo9pdXgLAAEE6AMAAAToAwAAUEsBAh4DFAAAAAgAD4RNXKd7NDE+AAAARAAAAAcAGAAAAAAAAAAAAKSBgQAAADAwMi5wbmdVVAUAAx1Sj2l1eAsAAQToAwAABOgDAABQSwUGAAAAAAIAAgCaAAAAAAEAAAAA'
const wallColumns = 3

type FeedItem = {
  src: string
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
    .flatMap((column) => column.map((item) => item.src))
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

test('basic upload -> wall -> album flow', async ({ page }) => {
  await fs.mkdir(samplesDir, { recursive: true })
  await ensureFixtureZips()

  await page.goto('/')
  await expect(page.getByTestId('wall-grid')).toBeVisible()
  await expect(page.getByTestId('masonry-column')).toHaveCount(3)
  await expect(page.getByTestId('wall-refresh')).toBeVisible()

  await page.getByTestId('upload-input').setInputFiles(zip1)
  await expect(page.getByTestId('upload-button')).toHaveText('+', { timeout: 90_000 })

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
  const firstOriginalLink = page.getByTestId('photo-open-original')
  await expect(firstOriginalLink).toBeVisible()
  await expect(firstOriginalLink).toHaveAttribute('target', '_blank')
  await expect(firstOriginalLink).toHaveAttribute('rel', 'noopener noreferrer')
  await expect(firstOriginalLink).toHaveAttribute('href', /^\/api\/image\/[^/]+\/\d+$/)
  expect(await firstOriginalLink.getAttribute('href')).toContain(`/api/image/${albumId}/`)
  await page.getByTestId('photo-back').click()
  await expect(page.getByTestId('album-grid')).toBeVisible()

  await page.getByTestId('album-back').click()
  await expect(page.getByTestId('wall-grid')).toBeVisible()
  await expect(page.getByTestId('columns-6')).toBeVisible()
  await page.screenshot({ path: path.join(samplesDir, '03-wall.png'), fullPage: true })

  await page.getByTestId('upload-input').setInputFiles(zip2)
  await expect(page.getByTestId('upload-button')).toHaveText('+', { timeout: 120_000 })
  await expect(page.getByTestId('wall-tile').first()).toBeVisible({ timeout: 60_000 })
  await page.screenshot({ path: path.join(samplesDir, '04-second-album.png'), fullPage: true })
})
