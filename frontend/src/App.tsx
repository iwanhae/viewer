import { BrowserRouter, Navigate, Route, Routes } from 'react-router-dom'
import { WallPage } from './pages/WallPage'
import { ViewerPage } from './pages/ViewerPage'

export default function App() {
  return (
    <BrowserRouter>
      <Routes>
        <Route path="/" element={<WallPage />} />
        <Route path="/album/:albumId" element={<ViewerPage />} />
        <Route path="*" element={<Navigate to="/" replace />} />
      </Routes>
    </BrowserRouter>
  )
}
