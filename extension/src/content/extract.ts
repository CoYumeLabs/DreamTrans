import JSZip from 'jszip'
import * as pdfjs from 'pdfjs-dist'
// Registers the worker's message handler on the main thread so pdf.js runs
// without spawning a Worker (content scripts cannot create one from an
// extension URL). Slides are small; this is fast enough.
import 'pdfjs-dist/build/pdf.worker.mjs'
import type { DerivedFigure, DerivedPage, FetchedFile } from '../shared/types'
import { domToText } from './discovery'

pdfjs.GlobalWorkerOptions.workerSrc = 'pdf.worker.mjs'

// Extract: file → pages of text, plus renders of figure pages. Runs entirely
// in the browser; the original bytes never leave it.

export interface ExtractOptions {
  renderFigures: boolean
  maxFigures: number
  renderWidth: number
}

export interface Extracted {
  pages: DerivedPage[]
  extractor: string
}

export async function sha256Hex(bytes: ArrayBuffer): Promise<string> {
  const digest = await crypto.subtle.digest('SHA-256', bytes)
  return Array.from(new Uint8Array(digest)).map((b) => b.toString(16).padStart(2, '0')).join('')
}

function canvasToPngBase64(canvas: HTMLCanvasElement): string {
  return canvas.toDataURL('image/png').replace(/^data:image\/png;base64,/, '')
}

const IMAGE_OPS = new Set<number>([
  pdfjs.OPS.paintImageXObject,
  pdfjs.OPS.paintInlineImageXObject,
  pdfjs.OPS.paintImageMaskXObject,
  pdfjs.OPS.paintImageXObjectRepeat,
])

/**
 * Share of the page covered by images, from the operator list. pdf.js paints
 * every image into the unit square under the current transform, so each one
 * covers |det(CTM)| of user space. Logos and icons on slide templates stay
 * far below the figure threshold; a chart or photo does not.
 */
export function imageCoverage(fnArray: number[], argsArray: unknown[][], pageArea: number): number {
  type Matrix = [number, number, number, number, number, number]
  const multiply = (m: Matrix, t: Matrix): Matrix => [
    m[0] * t[0] + m[2] * t[1], m[1] * t[0] + m[3] * t[1],
    m[0] * t[2] + m[2] * t[3], m[1] * t[2] + m[3] * t[3],
    m[0] * t[4] + m[2] * t[5] + m[4], m[1] * t[4] + m[3] * t[5] + m[5],
  ]
  let ctm: Matrix = [1, 0, 0, 1, 0, 0]
  const stack: Matrix[] = []
  let covered = 0
  for (let i = 0; i < fnArray.length; i += 1) {
    const fn = fnArray[i]
    const args = argsArray[i] ?? []
    if (fn === pdfjs.OPS.save) stack.push(ctm)
    else if (fn === pdfjs.OPS.restore) ctm = stack.pop() ?? ctm
    else if (fn === pdfjs.OPS.transform) ctm = multiply(ctm, args as Matrix)
    else if (fn === pdfjs.OPS.paintFormXObjectBegin) {
      stack.push(ctm)
      if (Array.isArray(args[0]) && args[0].length === 6) ctm = multiply(ctm, args[0] as Matrix)
    } else if (fn === pdfjs.OPS.paintFormXObjectEnd) ctm = stack.pop() ?? ctm
    else if (fn === pdfjs.OPS.paintImageXObjectRepeat) covered += pageArea
    else if (IMAGE_OPS.has(fn)) covered += Math.abs(ctm[0] * ctm[3] - ctm[1] * ctm[2])
  }
  return pageArea > 0 ? Math.min(1, covered / pageArea) : 0
}

/** Images covering at least this share of a page make it worth a render. */
const FIGURE_COVERAGE = 0.12

interface TextItemLike { str: string; transform: number[]; hasEOL?: boolean }

/** Joins pdf.js text items into lines by their y position. */
function itemsToText(items: TextItemLike[]): string {
  const lines: string[] = []
  let currentY: number | null = null
  let current: string[] = []
  for (const item of items) {
    if (!item.str) continue
    const y = Math.round(item.transform[5])
    if (currentY !== null && Math.abs(y - currentY) > 2) {
      lines.push(current.join(' '))
      current = []
    }
    currentY = y
    current.push(item.str.trim())
    if (item.hasEOL) {
      lines.push(current.join(' '))
      current = []
      currentY = null
    }
  }
  if (current.length) lines.push(current.join(' '))
  return lines.map((line) => line.replace(/\s+/g, ' ').trim()).filter(Boolean).join('\n')
}

async function extractPDF(bytes: ArrayBuffer, options: ExtractOptions): Promise<Extracted> {
  const pdf = await pdfjs.getDocument({ data: new Uint8Array(bytes), isEvalSupported: false, useSystemFonts: true }).promise
  const pages: DerivedPage[] = []
  let figuresRendered = 0
  for (let n = 1; n <= pdf.numPages; n += 1) {
    const page = await pdf.getPage(n)
    const textContent = await page.getTextContent()
    const text = itemsToText(textContent.items as TextItemLike[])
    const textless = text.replace(/\s/g, '').length < 40
    let coverage = 0
    if (options.renderFigures && !textless) {
      try {
        const ops = await page.getOperatorList()
        const [x0, y0, x1, y1] = page.view
        coverage = imageCoverage(ops.fnArray, ops.argsArray as unknown[][], Math.abs((x1 - x0) * (y1 - y0)))
      } catch {
        coverage = 0
      }
    }
    const figures: DerivedFigure[] = []
    // Scanned or image-only pages, or pages with a real figure. A slide whose
    // only image is the template logo already has its text layer.
    const figurePage = textless || coverage >= FIGURE_COVERAGE
    if (options.renderFigures && figurePage && figuresRendered < options.maxFigures) {
      const base = page.getViewport({ scale: 1 })
      const scale = options.renderWidth / base.width
      const viewport = page.getViewport({ scale })
      const canvas = document.createElement('canvas')
      canvas.width = Math.ceil(viewport.width)
      canvas.height = Math.ceil(viewport.height)
      const context = canvas.getContext('2d')
      if (context) {
        await page.render({ canvasContext: context, viewport }).promise
        figures.push({ png_base64: canvasToPngBase64(canvas), bbox: [0, 0, 1, 1] })
        figuresRendered += 1
      }
      canvas.width = 0
      canvas.height = 0
    }
    pages.push({ n, text, ...(figures.length ? { figures } : {}) })
    page.cleanup()
  }
  await pdf.destroy()
  return { pages, extractor: 'pdfjs' }
}

function xmlText(xml: string, tag: string): string[] {
  const pattern = new RegExp(`<${tag}(?:\\s[^>]*)?>([^<]*)</${tag}>`, 'g')
  const out: string[] = []
  for (const match of xml.matchAll(pattern)) {
    out.push(match[1].replace(/&lt;/g, '<').replace(/&gt;/g, '>').replace(/&amp;/g, '&').replace(/&quot;/g, '"').replace(/&apos;/g, "'"))
  }
  return out
}

async function extractPPTX(bytes: ArrayBuffer): Promise<Extracted> {
  const zip = await JSZip.loadAsync(bytes)
  const slides = Object.keys(zip.files)
    .map((name) => ({ name, n: Number(name.match(/^ppt\/slides\/slide(\d+)\.xml$/)?.[1] ?? 0) }))
    .filter((entry) => entry.n > 0)
    .sort((a, b) => a.n - b.n)
  const pages: DerivedPage[] = []
  for (const slide of slides) {
    const xml = await zip.file(slide.name)!.async('string')
    const paragraphs = xml.split(/<\/a:p>/).map((p) => xmlText(p, 'a:t').join('')).filter((line) => line.trim())
    const notesName = `ppt/notesSlides/notesSlide${slide.n}.xml`
    let notes = ''
    if (zip.file(notesName)) {
      notes = xmlText(await zip.file(notesName)!.async('string'), 'a:t').join(' ').trim()
    }
    const text = [paragraphs.join('\n'), notes ? `Notes: ${notes}` : ''].filter(Boolean).join('\n')
    pages.push({ n: slide.n, text })
  }
  return { pages, extractor: 'pptx-xml' }
}

async function extractDOCX(bytes: ArrayBuffer): Promise<Extracted> {
  const zip = await JSZip.loadAsync(bytes)
  const xml = await zip.file('word/document.xml')?.async('string')
  if (!xml) return { pages: [], extractor: 'docx-xml' }
  const paragraphs = xml.split(/<\/w:p>/).map((p) => xmlText(p, 'w:t').join('')).filter((line) => line.trim())
  // Word has no fixed pages; group ~60 paragraphs per page so citations stay usable.
  const pages: DerivedPage[] = []
  for (let index = 0; index < paragraphs.length; index += 60) {
    pages.push({ n: pages.length + 1, text: paragraphs.slice(index, index + 60).join('\n') })
  }
  return { pages, extractor: 'docx-xml' }
}

async function extractImage(bytes: ArrayBuffer, mimetype: string, options: ExtractOptions): Promise<Extracted> {
  if (!options.renderFigures) return { pages: [], extractor: 'image' }
  const blob = new Blob([bytes], { type: mimetype })
  const url = URL.createObjectURL(blob)
  try {
    const image = await new Promise<HTMLImageElement>((resolve, reject) => {
      const element = new Image()
      element.onload = () => resolve(element)
      element.onerror = () => reject(new Error('image decode failed'))
      element.src = url
    })
    const scale = Math.min(1, options.renderWidth / image.naturalWidth)
    const canvas = document.createElement('canvas')
    canvas.width = Math.ceil(image.naturalWidth * scale)
    canvas.height = Math.ceil(image.naturalHeight * scale)
    canvas.getContext('2d')?.drawImage(image, 0, 0, canvas.width, canvas.height)
    return { pages: [{ n: 1, text: '', figures: [{ png_base64: canvasToPngBase64(canvas), bbox: [0, 0, 1, 1] }] }], extractor: 'image' }
  } finally {
    URL.revokeObjectURL(url)
  }
}

function extractText(bytes: ArrayBuffer, mimetype: string): Extracted {
  const text = new TextDecoder().decode(bytes)
  const body = mimetype === 'text/html' ? domToText(new DOMParser().parseFromString(text, 'text/html').body) : text
  const chunks = body.split(/\n{3,}/).filter((chunk) => chunk.trim())
  const pages: DerivedPage[] = []
  let buffer = ''
  for (const chunk of chunks) {
    if (buffer.length + chunk.length > 6000 && buffer) {
      pages.push({ n: pages.length + 1, text: buffer.trim() })
      buffer = ''
    }
    buffer += `${chunk}\n\n`
  }
  if (buffer.trim()) pages.push({ n: pages.length + 1, text: buffer.trim() })
  return { pages, extractor: 'text' }
}

export async function extractFile(file: FetchedFile, options: ExtractOptions): Promise<Extracted | null> {
  const lower = file.filename.toLowerCase()
  const type = file.mimetype
  try {
    if (type === 'application/pdf' || lower.endsWith('.pdf')) return await extractPDF(file.bytes, options)
    if (lower.endsWith('.pptx') || type.includes('presentationml')) return await extractPPTX(file.bytes)
    if (lower.endsWith('.docx') || type.includes('wordprocessingml')) return await extractDOCX(file.bytes)
    if (type.startsWith('image/')) return await extractImage(file.bytes, type, options)
    if (type.startsWith('text/') || /\.(txt|md|csv|html?)$/.test(lower)) return extractText(file.bytes, type)
  } catch (reason) {
    throw new Error(`extract ${file.filename}: ${reason instanceof Error ? reason.message : String(reason)}`)
  }
  return null
}
