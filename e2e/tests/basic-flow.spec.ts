import { expect, test } from '@playwright/test'
import fs from 'node:fs/promises'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const here = path.dirname(fileURLToPath(import.meta.url))
const rootDir = path.resolve(here, '..', '..')
const samplesDir = process.env.SCREENSHOT_DIR ?? path.join(rootDir, 'samples')
const zip1 = path.join(rootDir, 'samples', '2a03f38ddcdc7281d8e53a3c.zip')
const zip2 = path.join(rootDir, 'samples', '5b4c3cc15ee5190007a11d0b.zip')

test('basic upload -> wall -> album flow', async ({ page }) => {
  await fs.mkdir(samplesDir, { recursive: true })

  await page.goto('/')
  await expect(page.getByTestId('wall-grid')).toBeVisible()

  await page.getByTestId('upload-input').setInputFiles(zip1)
  await expect(page.getByTestId('upload-button')).toHaveText('+', { timeout: 90_000 })

  await expect(page.getByTestId('wall-tile').first()).toBeVisible({ timeout: 60_000 })
  await page.screenshot({ path: path.join(samplesDir, '01-upload-finalize.png'), fullPage: true })

  await page.getByTestId('wall-tile').first().click()
  await expect(page.getByTestId('album-grid')).toBeVisible()
  await expect(page.getByTestId('album-tile').first()).toBeVisible()
  await expect(page.getByTestId('album-columns-4')).toBeVisible()
  await page.screenshot({ path: path.join(samplesDir, '02-album.png'), fullPage: true })

  await page.getByTestId('album-back').click()
  await expect(page.getByTestId('wall-grid')).toBeVisible()
  await page.screenshot({ path: path.join(samplesDir, '03-wall.png'), fullPage: true })

  await page.getByTestId('upload-input').setInputFiles(zip2)
  await expect(page.getByTestId('upload-button')).toHaveText('+', { timeout: 120_000 })
  await expect(page.getByTestId('wall-tile').first()).toBeVisible({ timeout: 60_000 })
  await page.screenshot({ path: path.join(samplesDir, '04-second-album.png'), fullPage: true })
})
