import { expect, test } from '@playwright/test'
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

async function ensureFixtureZips() {
  await fs.mkdir(fixturesDir, { recursive: true })
  await fs.writeFile(zip1, Buffer.from(zip1Base64, 'base64'))
  await fs.writeFile(zip2, Buffer.from(zip2Base64, 'base64'))
}

test('basic upload -> wall -> album flow', async ({ page }) => {
  await fs.mkdir(samplesDir, { recursive: true })
  await ensureFixtureZips()

  await page.goto('/')
  await expect(page.getByTestId('wall-grid')).toBeVisible()

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
  await page.screenshot({ path: path.join(samplesDir, '01-upload-finalize.png'), fullPage: true })

  await page.getByTestId('wall-tile').first().click()
  await expect(page.getByTestId('album-grid')).toBeVisible()
  await expect(page.getByTestId('album-tile').first()).toBeVisible()
  const albumPath = new URL(page.url()).pathname
  const albumId = albumPath.split('/').at(-1)
  expect(albumId).toBeTruthy()

  const firstOriginalLink = page.getByTestId('album-original-link').first()
  await expect(firstOriginalLink).toBeVisible()
  await expect(firstOriginalLink).toHaveAttribute('target', '_blank')
  await expect(firstOriginalLink).toHaveAttribute('rel', 'noopener noreferrer')
  await expect(firstOriginalLink).toHaveAttribute('href', /^\/api\/image\/[^/]+\/\d+$/)
  expect(await firstOriginalLink.getAttribute('href')).not.toContain('mode=wall')
  expect(await firstOriginalLink.getAttribute('href')).toContain(`/api/image/${albumId}/`)

  await expect(page.getByTestId('album-columns-4')).toBeVisible()
  await expect(page.getByTestId('album-columns-6')).toBeVisible()
  await page.screenshot({ path: path.join(samplesDir, '02-album.png'), fullPage: true })

  await page.getByTestId('album-back').click()
  await expect(page.getByTestId('wall-grid')).toBeVisible()
  await expect(page.getByTestId('columns-6')).toBeVisible()
  await page.screenshot({ path: path.join(samplesDir, '03-wall.png'), fullPage: true })

  await page.getByTestId('upload-input').setInputFiles(zip2)
  await expect(page.getByTestId('upload-button')).toHaveText('+', { timeout: 120_000 })
  await expect(page.getByTestId('wall-tile').first()).toBeVisible({ timeout: 60_000 })
  await page.screenshot({ path: path.join(samplesDir, '04-second-album.png'), fullPage: true })
})
