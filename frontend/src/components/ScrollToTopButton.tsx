import { useEffect, useRef, useState } from 'react'
import { useLocation } from 'react-router-dom'
import { UpIcon } from './IslandIcons'

const SHOW_THRESHOLD_PX = 200
const DIRECTION_DELTA_PX = 6

export function ScrollToTopButton() {
  const location = useLocation()
  const [visible, setVisible] = useState(false)
  const lastScrollYRef = useRef(0)

  useEffect(() => {
    if (typeof window === 'undefined') return
    lastScrollYRef.current = window.scrollY ?? 0
    setVisible(false)
  }, [location.pathname, location.search, location.hash])

  useEffect(() => {
    if (typeof window === 'undefined') return

    let ticking = false
    let frameID = 0
    const updateVisibility = () => {
      const currentY = window.scrollY ?? 0
      const delta = currentY - lastScrollYRef.current

      setVisible((previous) => {
        if (currentY <= SHOW_THRESHOLD_PX) return false
        if (delta <= -DIRECTION_DELTA_PX) return true
        if (delta >= DIRECTION_DELTA_PX) return false
        return previous
      })

      lastScrollYRef.current = currentY
      ticking = false
    }

    const onScroll = () => {
      if (ticking) return
      ticking = true
      frameID = requestAnimationFrame(updateVisibility)
    }

    window.addEventListener('scroll', onScroll, { passive: true })
    return () => {
      window.removeEventListener('scroll', onScroll)
      if (frameID !== 0) {
        cancelAnimationFrame(frameID)
      }
    }
  }, [])

  const onClick = () => {
    if (typeof window === 'undefined') return
    window.scrollTo({ top: 0, behavior: 'smooth' })
  }

  return (
    <button
      type="button"
      className={`scroll-top-button ${visible ? 'is-visible' : ''}`.trim()}
      aria-label="Go to top"
      data-testid="scroll-to-top"
      onClick={onClick}
    >
      <UpIcon />
    </button>
  )
}
